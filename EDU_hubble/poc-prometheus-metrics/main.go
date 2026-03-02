// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble Prometheus 메트릭 패턴
//
// Hubble은 Prometheus를 사용하여 다양한 메트릭을 수집합니다:
//   - Counter: 단조 증가 (예: 총 Flow 수)
//   - Gauge: 현재 값 (예: 연결된 Peer 수)
//   - Histogram: 분포 (예: 요청 지연 시간)
//
// 이 PoC는 외부 의존성 없이 Prometheus 메트릭 패턴을 시뮬레이션합니다.
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// ========================================
// 1. Prometheus 메트릭 타입 시뮬레이션
// ========================================

// Counter는 단조 증가하는 메트릭입니다.
// 리셋 없이 계속 증가합니다.
// 예: hubble_flows_processed_total
type Counter struct {
	mu     sync.Mutex
	name   string
	help   string
	labels map[string]float64 // label → value
}

func NewCounter(name, help string) *Counter {
	return &Counter{
		name:   name,
		help:   help,
		labels: make(map[string]float64),
	}
}

func (c *Counter) Inc(label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.labels[label]++
}

func (c *Counter) Add(label string, val float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.labels[label] += val
}

// Gauge는 현재 값을 나타내는 메트릭입니다.
// 증가/감소 모두 가능합니다.
// 예: hubble_relay_pool_peer_connection_status
type Gauge struct {
	mu     sync.Mutex
	name   string
	help   string
	labels map[string]float64
}

func NewGauge(name, help string) *Gauge {
	return &Gauge{
		name:   name,
		help:   help,
		labels: make(map[string]float64),
	}
}

func (g *Gauge) Set(label string, val float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.labels[label] = val
}

func (g *Gauge) Inc(label string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.labels[label]++
}

func (g *Gauge) Dec(label string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.labels[label]--
}

// Histogram은 값의 분포를 추적하는 메트릭입니다.
// 미리 정의된 버킷에 값을 분류합니다.
// 예: hubble_grpc_request_duration_seconds
type Histogram struct {
	mu      sync.Mutex
	name    string
	help    string
	buckets []float64
	counts  []uint64 // 각 버킷의 누적 카운트
	sum     float64
	count   uint64
}

func NewHistogram(name, help string, buckets []float64) *Histogram {
	sort.Float64s(buckets)
	return &Histogram{
		name:    name,
		help:    help,
		buckets: buckets,
		counts:  make([]uint64, len(buckets)),
	}
}

func (h *Histogram) Observe(val float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sum += val
	h.count++
	for i, bucket := range h.buckets {
		if val <= bucket {
			h.counts[i]++
		}
	}
}

// ========================================
// 2. Registry (메트릭 저장소)
// ========================================

// Registry는 모든 메트릭을 관리합니다.
// 실제 Prometheus: prometheus.NewPedanticRegistry()
type Registry struct {
	mu         sync.Mutex
	counters   []*Counter
	gauges     []*Gauge
	histograms []*Histogram
}

func NewRegistry() *Registry {
	return &Registry{}
}

func (r *Registry) RegisterCounter(c *Counter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters = append(r.counters, c)
}

func (r *Registry) RegisterGauge(g *Gauge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges = append(r.gauges, g)
}

func (r *Registry) RegisterHistogram(h *Histogram) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.histograms = append(r.histograms, h)
}

// Expose는 Prometheus 텍스트 형식으로 메트릭을 출력합니다.
// 실제로는 /metrics HTTP 엔드포인트에서 제공됩니다.
func (r *Registry) Expose() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var sb strings.Builder

	for _, c := range r.counters {
		c.mu.Lock()
		sb.WriteString(fmt.Sprintf("# HELP %s %s\n", c.name, c.help))
		sb.WriteString(fmt.Sprintf("# TYPE %s counter\n", c.name))
		for label, val := range c.labels {
			sb.WriteString(fmt.Sprintf("%s{type=%q} %g\n", c.name, label, val))
		}
		c.mu.Unlock()
		sb.WriteString("\n")
	}

	for _, g := range r.gauges {
		g.mu.Lock()
		sb.WriteString(fmt.Sprintf("# HELP %s %s\n", g.name, g.help))
		sb.WriteString(fmt.Sprintf("# TYPE %s gauge\n", g.name))
		for label, val := range g.labels {
			sb.WriteString(fmt.Sprintf("%s{status=%q} %g\n", g.name, label, val))
		}
		g.mu.Unlock()
		sb.WriteString("\n")
	}

	for _, h := range r.histograms {
		h.mu.Lock()
		sb.WriteString(fmt.Sprintf("# HELP %s %s\n", h.name, h.help))
		sb.WriteString(fmt.Sprintf("# TYPE %s histogram\n", h.name))
		for i, bucket := range h.buckets {
			if bucket == math.Inf(1) {
				sb.WriteString(fmt.Sprintf("%s_bucket{le=\"+Inf\"} %d\n", h.name, h.counts[i]))
			} else {
				sb.WriteString(fmt.Sprintf("%s_bucket{le=\"%g\"} %d\n", h.name, bucket, h.counts[i]))
			}
		}
		sb.WriteString(fmt.Sprintf("%s_sum %g\n", h.name, h.sum))
		sb.WriteString(fmt.Sprintf("%s_count %d\n", h.name, h.count))
		h.mu.Unlock()
		sb.WriteString("\n")
	}

	return sb.String()
}

// ========================================
// 3. Hubble 메트릭 정의
// ========================================

// HubbleMetrics는 Hubble에서 사용하는 주요 메트릭들입니다.
type HubbleMetrics struct {
	FlowsProcessed  *Counter
	PeerConnStatus   *Gauge
	RequestDuration  *Histogram
	DroppedFlows     *Counter
}

func NewHubbleMetrics(registry *Registry) *HubbleMetrics {
	m := &HubbleMetrics{
		FlowsProcessed: NewCounter(
			"hubble_flows_processed_total",
			"Total number of flows processed",
		),
		PeerConnStatus: NewGauge(
			"hubble_relay_pool_peer_connection_status",
			"Current peer connection status",
		),
		RequestDuration: NewHistogram(
			"hubble_grpc_request_duration_seconds",
			"Duration of gRPC requests in seconds",
			[]float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0, math.Inf(1)},
		),
		DroppedFlows: NewCounter(
			"hubble_dropped_flows_total",
			"Total number of dropped flows",
		),
	}

	registry.RegisterCounter(m.FlowsProcessed)
	registry.RegisterGauge(m.PeerConnStatus)
	registry.RegisterHistogram(m.RequestDuration)
	registry.RegisterCounter(m.DroppedFlows)

	return m
}

// ========================================
// 4. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble Prometheus 메트릭 패턴 ===")
	fmt.Println()
	fmt.Println("메트릭 타입:")
	fmt.Println("  Counter:   단조 증가 (예: 총 Flow 수)")
	fmt.Println("  Gauge:     현재 값 (예: 연결된 Peer 수)")
	fmt.Println("  Histogram: 분포 (예: 요청 지연 시간)")
	fmt.Println()

	registry := NewRegistry()
	metrics := NewHubbleMetrics(registry)

	// ── 시나리오 1: Flow 처리 시뮬레이션 ──
	fmt.Println("━━━ 시나리오 1: Flow 처리 시뮬레이션 ━━━")
	fmt.Println()

	flows := []struct {
		verdict  string
		protocol string
	}{
		{"FORWARDED", "TCP"},
		{"FORWARDED", "TCP"},
		{"DROPPED", "UDP"},
		{"FORWARDED", "DNS"},
		{"DROPPED", "TCP"},
		{"FORWARDED", "HTTP"},
		{"FORWARDED", "TCP"},
		{"DROPPED", "TCP"},
		{"FORWARDED", "DNS"},
		{"FORWARDED", "TCP"},
	}

	for _, f := range flows {
		metrics.FlowsProcessed.Inc(f.verdict)
		if f.verdict == "DROPPED" {
			metrics.DroppedFlows.Inc(f.protocol)
		}
	}

	fmt.Printf("  처리된 Flow: %d개\n", len(flows))
	fmt.Println()

	// ── 시나리오 2: Peer 연결 상태 ──
	fmt.Println("━━━ 시나리오 2: Peer 연결 상태 (Gauge) ━━━")
	fmt.Println()

	// 초기 상태
	metrics.PeerConnStatus.Set("READY", 3)
	metrics.PeerConnStatus.Set("CONNECTING", 1)
	metrics.PeerConnStatus.Set("IDLE", 1)
	fmt.Println("  초기 상태: READY=3, CONNECTING=1, IDLE=1")

	// Peer 연결 변화
	metrics.PeerConnStatus.Dec("CONNECTING")
	metrics.PeerConnStatus.Inc("READY")
	fmt.Println("  변화 후:   CONNECTING-1, READY+1")
	fmt.Println()

	// ── 시나리오 3: 요청 지연시간 (Histogram) ──
	fmt.Println("━━━ 시나리오 3: gRPC 요청 지연시간 (Histogram) ━━━")
	fmt.Println()

	durations := []float64{
		0.002, 0.003, 0.008, 0.012, 0.045,
		0.003, 0.078, 0.150, 0.005, 0.001,
	}

	for _, d := range durations {
		metrics.RequestDuration.Observe(d)
	}

	fmt.Println("  요청 지연시간 분포:")
	for i, bucket := range metrics.RequestDuration.buckets {
		bar := strings.Repeat("█", int(metrics.RequestDuration.counts[i]))
		if bucket == math.Inf(1) {
			fmt.Printf("    ≤ +Inf  : %s (%d)\n", bar, metrics.RequestDuration.counts[i])
		} else {
			fmt.Printf("    ≤ %5.3fs: %s (%d)\n", bucket, bar, metrics.RequestDuration.counts[i])
		}
	}
	fmt.Printf("    평균: %.3fs\n", metrics.RequestDuration.sum/float64(metrics.RequestDuration.count))
	fmt.Println()

	// ── Prometheus 텍스트 형식 출력 ──
	fmt.Println("━━━ /metrics 엔드포인트 출력 (Prometheus 텍스트 형식) ━━━")
	fmt.Println()

	output := registry.Expose()
	for _, line := range strings.Split(output, "\n") {
		fmt.Printf("  %s\n", line)
	}

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - Counter: 리셋 없이 단조 증가 (rate() 함수로 초당 변화율 계산)")
	fmt.Println("  - Gauge: 현재 값 (증가/감소 가능)")
	fmt.Println("  - Histogram: 버킷별 분포 + sum/count (평균 계산 가능)")
	fmt.Println("  - Registry: 모든 메트릭 중앙 관리")
	fmt.Println("  - 실제 Hubble: /metrics HTTP 엔드포인트로 Prometheus가 스크레이핑")

	_ = time.Now() // time 패키지 사용
}
