package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// PoC 10: DNS 메트릭 (DNS Metrics)
// =============================================================================
// CoreDNS의 plugin/metrics/vars/vars.go에서 정의된 Prometheus 메트릭 수집 시스템을
// 시뮬레이션한다. CoreDNS는 prometheus 플러그인을 통해 요청/응답 통계를 수집하고
// /metrics 엔드포인트로 노출한다.
//
// 참조: coredns/plugin/metrics/vars/vars.go
//       - RequestCount: requests_total 카운터
//       - ResponseRcode: responses_total 카운터
//       - RequestDuration: request_duration_seconds 히스토그램
//       coredns/plugin/metrics/vars/report.go
//       - Report(): 메트릭 데이터 수집 함수
// =============================================================================

// =============================================================================
// 메트릭 타입 정의
// =============================================================================

// Counter는 Prometheus Counter 메트릭을 시뮬레이션한다.
// 단조 증가하는 값만 허용한다.
type Counter struct {
	mu     sync.Mutex
	values map[string]float64 // 레이블 조합 → 값
	name   string
	help   string
	labels []string
}

// NewCounter는 새로운 Counter를 생성한다.
func NewCounter(name, help string, labels []string) *Counter {
	return &Counter{
		values: make(map[string]float64),
		name:   name,
		help:   help,
		labels: labels,
	}
}

// Inc는 카운터를 1 증가시킨다.
func (c *Counter) Inc(labelValues ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := strings.Join(labelValues, "|")
	c.values[key]++
}

// Histogram은 Prometheus Histogram 메트릭을 시뮬레이션한다.
// 관측값을 버킷에 분배하고 합계/개수를 추적한다.
type Histogram struct {
	mu      sync.Mutex
	buckets []float64
	data    map[string]*histogramData // 레이블 조합 → 데이터
	name    string
	help    string
	labels  []string
}

type histogramData struct {
	counts map[float64]uint64 // 버킷 상한 → 누적 카운트
	sum    float64
	count  uint64
}

// NewHistogram은 새로운 Histogram을 생성한다.
func NewHistogram(name, help string, labels []string, buckets []float64) *Histogram {
	sorted := make([]float64, len(buckets))
	copy(sorted, buckets)
	sort.Float64s(sorted)
	return &Histogram{
		buckets: sorted,
		data:    make(map[string]*histogramData),
		name:    name,
		help:    help,
		labels:  labels,
	}
}

// Observe는 관측값을 히스토그램에 기록한다.
func (h *Histogram) Observe(value float64, labelValues ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	key := strings.Join(labelValues, "|")
	d, ok := h.data[key]
	if !ok {
		d = &histogramData{
			counts: make(map[float64]uint64),
		}
		h.data[key] = d
	}

	d.sum += value
	d.count++

	// 버킷에 분배 (해당하는 가장 작은 버킷에만 기록, 출력 시 누적 합산)
	for _, bound := range h.buckets {
		if value <= bound {
			d.counts[bound]++
			break
		}
	}
}

// Gauge는 Prometheus Gauge 메트릭을 시뮬레이션한다.
type Gauge struct {
	mu     sync.Mutex
	values map[string]float64
	name   string
	help   string
	labels []string
}

// NewGauge는 새로운 Gauge를 생성한다.
func NewGauge(name, help string, labels []string) *Gauge {
	return &Gauge{
		values: make(map[string]float64),
		name:   name,
		help:   help,
		labels: labels,
	}
}

// Set은 게이지 값을 설정한다.
func (g *Gauge) Set(value float64, labelValues ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := strings.Join(labelValues, "|")
	g.values[key] = value
}

// =============================================================================
// CoreDNS 메트릭 레지스트리
// CoreDNS의 vars.go에서 정의된 메트릭들을 동일한 구조로 재현한다.
// =============================================================================

// MetricsRegistry는 CoreDNS의 DNS 메트릭 레지스트리를 시뮬레이션한다.
type MetricsRegistry struct {
	RequestCount    *Counter   // coredns_dns_requests_total
	ResponseRcode   *Counter   // coredns_dns_responses_total
	RequestDuration *Histogram // coredns_dns_request_duration_seconds
	RequestSize     *Histogram // coredns_dns_request_size_bytes
	ResponseSize    *Histogram // coredns_dns_response_size_bytes
	RequestDo       *Counter   // coredns_dns_do_requests_total
	PluginEnabled   *Gauge     // coredns_plugin_enabled
	PanicsTotal     *Counter   // coredns_panics_total
}

// NewMetricsRegistry는 CoreDNS와 동일한 메트릭 세트를 초기화한다.
func NewMetricsRegistry() *MetricsRegistry {
	// CoreDNS의 plugin.TimeBuckets와 유사한 버킷
	timeBuckets := []float64{
		0.00025, 0.0005, 0.001, 0.002, 0.004, 0.008, 0.016, 0.032,
		0.064, 0.128, 0.256, 0.512, 1.0, 2.0, 4.0, 8.0,
	}

	sizeBuckets := []float64{
		0, 100, 200, 300, 400, 511, 1023, 2047, 4095, 8291, 16000, 32000, 48000, 64000,
	}

	return &MetricsRegistry{
		RequestCount: NewCounter(
			"coredns_dns_requests_total",
			"Counter of DNS requests made per zone, protocol and family.",
			[]string{"server", "zone", "proto", "family", "type"},
		),
		ResponseRcode: NewCounter(
			"coredns_dns_responses_total",
			"Counter of response status codes.",
			[]string{"server", "zone", "rcode", "plugin"},
		),
		RequestDuration: NewHistogram(
			"coredns_dns_request_duration_seconds",
			"Histogram of the time (in seconds) each request took per zone.",
			[]string{"server", "zone"},
			timeBuckets,
		),
		RequestSize: NewHistogram(
			"coredns_dns_request_size_bytes",
			"Size of the EDNS0 UDP buffer in bytes (64K for TCP) per zone and protocol.",
			[]string{"server", "zone", "proto"},
			sizeBuckets,
		),
		ResponseSize: NewHistogram(
			"coredns_dns_response_size_bytes",
			"Size of the returned response in bytes.",
			[]string{"server", "zone", "proto"},
			sizeBuckets,
		),
		RequestDo: NewCounter(
			"coredns_dns_do_requests_total",
			"Counter of DNS requests with DO bit set per zone.",
			[]string{"server", "zone"},
		),
		PluginEnabled: NewGauge(
			"coredns_plugin_enabled",
			"A metric that indicates whether a plugin is enabled.",
			[]string{"server", "zone", "name"},
		),
		PanicsTotal: NewCounter(
			"coredns_panics_total",
			"A metric that counts the number of panics.",
			nil,
		),
	}
}

// Report는 CoreDNS의 vars.Report()와 동일한 메트릭 수집을 수행한다.
// server, zone, proto, family, qtype, rcode, plugin, size, duration 정보를 기록한다.
func (m *MetricsRegistry) Report(server, zone, proto, family, qtype, rcode, pluginName string,
	reqSize, respSize int, duration time.Duration, doBit bool) {

	m.RequestCount.Inc(server, zone, proto, family, qtype)
	m.ResponseRcode.Inc(server, zone, rcode, pluginName)
	m.RequestDuration.Observe(duration.Seconds(), server, zone)
	m.RequestSize.Observe(float64(reqSize), server, zone, proto)
	m.ResponseSize.Observe(float64(respSize), server, zone, proto)

	if doBit {
		m.RequestDo.Inc(server, zone)
	}
}

// =============================================================================
// Prometheus 형식 출력
// =============================================================================

// FormatPrometheus는 모든 메트릭을 Prometheus exposition 형식으로 출력한다.
func (m *MetricsRegistry) FormatPrometheus() string {
	var sb strings.Builder

	// Counter 출력
	formatCounter(&sb, m.RequestCount)
	formatCounter(&sb, m.ResponseRcode)
	formatCounter(&sb, m.RequestDo)
	formatCounter(&sb, m.PanicsTotal)

	// Histogram 출력
	formatHistogram(&sb, m.RequestDuration)
	formatHistogram(&sb, m.RequestSize)
	formatHistogram(&sb, m.ResponseSize)

	// Gauge 출력
	formatGauge(&sb, m.PluginEnabled)

	return sb.String()
}

func formatCounter(sb *strings.Builder, c *Counter) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.values) == 0 {
		return
	}

	sb.WriteString(fmt.Sprintf("# HELP %s %s\n", c.name, c.help))
	sb.WriteString(fmt.Sprintf("# TYPE %s counter\n", c.name))

	keys := sortedKeys(c.values)
	for _, key := range keys {
		val := c.values[key]
		if len(c.labels) == 0 {
			sb.WriteString(fmt.Sprintf("%s %.0f\n", c.name, val))
		} else {
			labelStr := formatLabels(c.labels, strings.Split(key, "|"))
			sb.WriteString(fmt.Sprintf("%s{%s} %.0f\n", c.name, labelStr, val))
		}
	}
	sb.WriteString("\n")
}

func formatHistogram(sb *strings.Builder, h *Histogram) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.data) == 0 {
		return
	}

	sb.WriteString(fmt.Sprintf("# HELP %s %s\n", h.name, h.help))
	sb.WriteString(fmt.Sprintf("# TYPE %s histogram\n", h.name))

	keys := make([]string, 0, len(h.data))
	for k := range h.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		d := h.data[key]
		labelParts := strings.Split(key, "|")
		labelStr := formatLabels(h.labels, labelParts)

		// 버킷 출력 (누적)
		cumulative := uint64(0)
		for _, bound := range h.buckets {
			cumulative += d.counts[bound]
			if labelStr != "" {
				sb.WriteString(fmt.Sprintf("%s_bucket{%s,le=\"%s\"} %d\n",
					h.name, labelStr, formatFloat(bound), cumulative))
			} else {
				sb.WriteString(fmt.Sprintf("%s_bucket{le=\"%s\"} %d\n",
					h.name, formatFloat(bound), cumulative))
			}
		}
		// +Inf 버킷
		if labelStr != "" {
			sb.WriteString(fmt.Sprintf("%s_bucket{%s,le=\"+Inf\"} %d\n", h.name, labelStr, d.count))
			sb.WriteString(fmt.Sprintf("%s_sum{%s} %f\n", h.name, labelStr, d.sum))
			sb.WriteString(fmt.Sprintf("%s_count{%s} %d\n", h.name, labelStr, d.count))
		} else {
			sb.WriteString(fmt.Sprintf("%s_bucket{le=\"+Inf\"} %d\n", h.name, d.count))
			sb.WriteString(fmt.Sprintf("%s_sum %f\n", h.name, d.sum))
			sb.WriteString(fmt.Sprintf("%s_count %d\n", h.name, d.count))
		}
	}
	sb.WriteString("\n")
}

func formatGauge(sb *strings.Builder, g *Gauge) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.values) == 0 {
		return
	}

	sb.WriteString(fmt.Sprintf("# HELP %s %s\n", g.name, g.help))
	sb.WriteString(fmt.Sprintf("# TYPE %s gauge\n", g.name))

	keys := sortedKeys(g.values)
	for _, key := range keys {
		val := g.values[key]
		labelStr := formatLabels(g.labels, strings.Split(key, "|"))
		sb.WriteString(fmt.Sprintf("%s{%s} %.0f\n", g.name, labelStr, val))
	}
	sb.WriteString("\n")
}

func formatLabels(names []string, values []string) string {
	if len(names) == 0 || len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names))
	for i, name := range names {
		val := ""
		if i < len(values) {
			val = values[i]
		}
		parts = append(parts, fmt.Sprintf("%s=\"%s\"", name, val))
	}
	return strings.Join(parts, ",")
}

func formatFloat(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%.1f", f)
	}
	return fmt.Sprintf("%g", f)
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// =============================================================================
// DNS 쿼리 시뮬레이션
// =============================================================================

// SimulatedQuery는 시뮬레이션된 DNS 쿼리를 나타낸다.
type SimulatedQuery struct {
	Server  string
	Zone    string
	QName   string
	QType   string
	Proto   string
	Family  string
	Rcode   string
	Plugin  string
	ReqSize int
	ResSize int
	DoBit   bool
}

func main() {
	fmt.Println("=== CoreDNS DNS 메트릭 (DNS Metrics) PoC ===")
	fmt.Println()

	registry := NewMetricsRegistry()

	// =========================================================================
	// 1. 플러그인 활성화 메트릭 등록
	// =========================================================================
	fmt.Println("--- 1. 플러그인 활성화 등록 ---")

	plugins := []string{"cache", "forward", "file", "metrics", "log", "errors"}
	for _, p := range plugins {
		registry.PluginEnabled.Set(1, "dns://:53", "example.com.", p)
		fmt.Printf("  플러그인 활성화: %s (server=dns://:53, zone=example.com.)\n", p)
	}

	// =========================================================================
	// 2. DNS 쿼리 시뮬레이션 및 메트릭 수집
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 2. DNS 쿼리 시뮬레이션 (50개 쿼리) ---")

	rng := rand.New(rand.NewSource(42))

	// 시뮬레이션 파라미터
	zones := []string{"example.com.", "test.io.", "."}
	qtypes := []string{"A", "AAAA", "CNAME", "MX", "PTR", "SRV", "TXT"}
	protos := []string{"udp", "tcp"}
	families := []string{"1", "2"} // 1=IPv4, 2=IPv6
	rcodes := []string{"NOERROR", "NOERROR", "NOERROR", "NOERROR", "NXDOMAIN", "SERVFAIL"}
	pluginNames := []string{"cache", "forward", "file"}

	queryCount := 50
	for i := 0; i < queryCount; i++ {
		q := SimulatedQuery{
			Server:  "dns://:53",
			Zone:    zones[rng.Intn(len(zones))],
			QType:   qtypes[rng.Intn(len(qtypes))],
			Proto:   protos[rng.Intn(len(protos))],
			Family:  families[rng.Intn(len(families))],
			Rcode:   rcodes[rng.Intn(len(rcodes))],
			Plugin:  pluginNames[rng.Intn(len(pluginNames))],
			ReqSize: 40 + rng.Intn(200),
			ResSize: 60 + rng.Intn(500),
			DoBit:   rng.Float64() < 0.3, // 30% 확률로 DO 비트 설정
		}

		// 응답 시간 시뮬레이션 (대부분 빠르고, 가끔 느림)
		var duration time.Duration
		if rng.Float64() < 0.9 {
			duration = time.Duration(rng.Intn(10)) * time.Millisecond // 0~10ms
		} else {
			duration = time.Duration(50+rng.Intn(200)) * time.Millisecond // 50~250ms (느린 쿼리)
		}

		registry.Report(q.Server, q.Zone, q.Proto, q.Family, q.QType,
			q.Rcode, q.Plugin, q.ReqSize, q.ResSize, duration, q.DoBit)
	}

	fmt.Printf("  총 %d개 쿼리 처리 완료\n", queryCount)

	// =========================================================================
	// 3. 메트릭 요약 통계
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 3. 메트릭 요약 통계 ---")

	// 요청 카운트 요약
	fmt.Println()
	fmt.Println("  [requests_total] Zone별 요청 수:")
	registry.RequestCount.mu.Lock()
	zoneCounts := make(map[string]float64)
	for key, val := range registry.RequestCount.values {
		parts := strings.Split(key, "|")
		if len(parts) >= 2 {
			zoneCounts[parts[1]] += val
		}
	}
	registry.RequestCount.mu.Unlock()
	for zone, count := range zoneCounts {
		fmt.Printf("    zone=%-15s → %3.0f 요청\n", zone, count)
	}

	// 응답 코드 요약
	fmt.Println()
	fmt.Println("  [responses_total] Rcode별 응답 수:")
	registry.ResponseRcode.mu.Lock()
	rcodeCounts := make(map[string]float64)
	for key, val := range registry.ResponseRcode.values {
		parts := strings.Split(key, "|")
		if len(parts) >= 3 {
			rcodeCounts[parts[2]] += val
		}
	}
	registry.ResponseRcode.mu.Unlock()
	for rcode, count := range rcodeCounts {
		fmt.Printf("    rcode=%-12s → %3.0f 응답\n", rcode, count)
	}

	// 응답 시간 히스토그램 요약
	fmt.Println()
	fmt.Println("  [request_duration_seconds] 응답 시간 분포:")
	registry.RequestDuration.mu.Lock()
	for key, d := range registry.RequestDuration.data {
		parts := strings.Split(key, "|")
		label := strings.Join(parts, ", ")
		avg := d.sum / float64(d.count)
		fmt.Printf("    {%s}:\n", label)
		fmt.Printf("      총 요청: %d, 합계: %.4fs, 평균: %.4fs\n", d.count, d.sum, avg)

		// 주요 버킷 출력
		cumulative := uint64(0)
		fmt.Printf("      버킷 분포:\n")
		selectedBuckets := []float64{0.001, 0.004, 0.008, 0.016, 0.064, 0.128, 0.256, 1.0}
		for _, bound := range selectedBuckets {
			cumulative = 0
			for _, b := range registry.RequestDuration.buckets {
				if b <= bound {
					cumulative += d.counts[b]
				}
			}
			pct := float64(cumulative) / float64(d.count) * 100
			bar := strings.Repeat("█", int(pct/5))
			fmt.Printf("        le=%-8s %3d/%d (%5.1f%%) %s\n",
				formatFloat(bound), cumulative, d.count, pct, bar)
		}
	}
	registry.RequestDuration.mu.Unlock()

	// =========================================================================
	// 4. Prometheus exposition 형식 출력
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 4. Prometheus /metrics 출력 (일부) ---")
	fmt.Println()

	output := registry.FormatPrometheus()
	// 출력이 길어지므로 일부만 표시
	lines := strings.Split(output, "\n")
	maxLines := 60
	if len(lines) > maxLines {
		for _, line := range lines[:maxLines] {
			fmt.Println(line)
		}
		fmt.Printf("\n... (총 %d줄 중 %d줄 표시)\n", len(lines), maxLines)
	} else {
		fmt.Print(output)
	}
}
