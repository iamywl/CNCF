package main

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Hubble 메트릭 핸들러 시뮬레이션
//
// 실제 구현 참조:
//   cilium/pkg/hubble/metrics/flow_processor.go  - StaticFlowProcessor
//   cilium/pkg/hubble/metrics/api/api.go         - Handler 인터페이스
//   cilium/pkg/hubble/metrics/dns/handler.go     - dnsHandler.ProcessFlow()
//   cilium/pkg/hubble/metrics/http/handler.go    - httpHandler.ProcessFlow()
//
// 핵심 개념:
//   1. MetricHandler 인터페이스: Init/ProcessFlow/Status/Deinit
//   2. FlowProcessor: 모든 핸들러에 대해 ProcessFlow 순차 호출
//   3. 프로토콜별 메트릭 추출: DNS, HTTP, TCP, Drop 각각의 메트릭
//   4. Prometheus 카운터/히스토그램 시뮬레이션
// =============================================================================

// --- Flow 데이터 모델 ---

type Verdict string

const (
	VerdictForwarded Verdict = "FORWARDED"
	VerdictDropped   Verdict = "DROPPED"
)

type TrafficDirection string

const (
	DirectionIngress TrafficDirection = "INGRESS"
	DirectionEgress  TrafficDirection = "EGRESS"
)

type L7FlowType string

const (
	L7Request  L7FlowType = "REQUEST"
	L7Response L7FlowType = "RESPONSE"
)

// DNSInfo는 DNS 관련 Flow 정보
type DNSInfo struct {
	Query   string
	QTypes  []string // A, AAAA, CNAME 등
	RCode   string   // NOERROR, NXDOMAIN 등
	RRTypes []string // A, AAAA 등
	IPs     []string
}

// HTTPInfo는 HTTP 관련 Flow 정보
type HTTPInfo struct {
	Method    string
	URL       string
	Protocol  string
	Code      int
	LatencyNs int64
}

// L7Info는 L7 프로토콜 정보
type L7Info struct {
	Type L7FlowType
	DNS  *DNSInfo
	HTTP *HTTPInfo
}

// Flow는 네트워크 이벤트
type Flow struct {
	Time             time.Time
	Source           string
	SourceNamespace  string
	Destination      string
	DestNamespace    string
	Verdict          Verdict
	TrafficDirection TrafficDirection
	IsReply          bool
	L7               *L7Info
	DropReason       string
}

// --- Prometheus 메트릭 시뮬레이션 ---

// Counter는 Prometheus Counter를 시뮬레이션
type Counter struct {
	value atomic.Int64
}

func (c *Counter) Inc() {
	c.value.Add(1)
}

func (c *Counter) Get() int64 {
	return c.value.Load()
}

// Histogram은 Prometheus Histogram을 시뮬레이션
type Histogram struct {
	mu      sync.Mutex
	count   int64
	sum     float64
	buckets map[float64]int64 // upper bound -> count
}

func NewHistogram(bounds []float64) *Histogram {
	h := &Histogram{buckets: make(map[float64]int64)}
	for _, b := range bounds {
		h.buckets[b] = 0
	}
	return h
}

func (h *Histogram) Observe(value float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += value
	for bound := range h.buckets {
		if value <= bound {
			h.buckets[bound]++
		}
	}
}

// CounterVec는 라벨이 있는 Counter 모음
type CounterVec struct {
	mu       sync.RWMutex
	counters map[string]*Counter
	name     string
}

func NewCounterVec(name string) *CounterVec {
	return &CounterVec{
		counters: make(map[string]*Counter),
		name:     name,
	}
}

func (cv *CounterVec) WithLabels(labels ...string) *Counter {
	key := strings.Join(labels, "|")
	cv.mu.Lock()
	defer cv.mu.Unlock()
	if c, ok := cv.counters[key]; ok {
		return c
	}
	c := &Counter{}
	cv.counters[key] = c
	return c
}

func (cv *CounterVec) Print() {
	cv.mu.RLock()
	defer cv.mu.RUnlock()
	if len(cv.counters) == 0 {
		return
	}
	fmt.Printf("  [%s]\n", cv.name)
	keys := make([]string, 0, len(cv.counters))
	for k := range cv.counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := cv.counters[k]
		fmt.Printf("    {%s} = %d\n", k, c.Get())
	}
}

// HistogramVec는 라벨이 있는 Histogram 모음
type HistogramVec struct {
	mu         sync.RWMutex
	histograms map[string]*Histogram
	name       string
	bounds     []float64
}

func NewHistogramVec(name string, bounds []float64) *HistogramVec {
	return &HistogramVec{
		histograms: make(map[string]*Histogram),
		name:       name,
		bounds:     bounds,
	}
}

func (hv *HistogramVec) WithLabels(labels ...string) *Histogram {
	key := strings.Join(labels, "|")
	hv.mu.Lock()
	defer hv.mu.Unlock()
	if h, ok := hv.histograms[key]; ok {
		return h
	}
	h := NewHistogram(hv.bounds)
	hv.histograms[key] = h
	return h
}

func (hv *HistogramVec) Print() {
	hv.mu.RLock()
	defer hv.mu.RUnlock()
	if len(hv.histograms) == 0 {
		return
	}
	fmt.Printf("  [%s]\n", hv.name)
	keys := make([]string, 0, len(hv.histograms))
	for k := range hv.histograms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h := hv.histograms[k]
		h.mu.Lock()
		fmt.Printf("    {%s} count=%d sum=%.4f\n", k, h.count, h.sum)
		h.mu.Unlock()
	}
}

// --- MetricHandler 인터페이스 ---
// 실제: api.Handler

type MetricHandler interface {
	Init() error
	ProcessFlow(ctx context.Context, flow *Flow) error
	Status() string
	Deinit()
}

// --- DNS 메트릭 핸들러 ---
// 실제: dns.dnsHandler

type DNSHandler struct {
	queries       *CounterVec
	responses     *CounterVec
	responseTypes *CounterVec
}

func NewDNSHandler() *DNSHandler {
	return &DNSHandler{}
}

func (h *DNSHandler) Init() error {
	// 실제: prometheus.NewCounterVec + registry.MustRegister
	h.queries = NewCounterVec("hubble_dns_queries_total")
	h.responses = NewCounterVec("hubble_dns_responses_total")
	h.responseTypes = NewCounterVec("hubble_dns_response_types_total")
	return nil
}

// ProcessFlow는 DNS Flow에서 메트릭을 추출
// 실제: dnsHandler.ProcessFlow()
func (h *DNSHandler) ProcessFlow(ctx context.Context, flow *Flow) error {
	if flow.L7 == nil {
		return nil
	}
	dns := flow.L7.DNS
	if dns == nil {
		return nil
	}

	qtypes := strings.Join(dns.QTypes, ",")
	ipsReturned := fmt.Sprintf("%d", len(dns.IPs))

	switch {
	case flow.Verdict == VerdictDropped:
		// 정책에 의해 차단된 DNS 쿼리
		h.queries.WithLabels("Policy denied", qtypes, ipsReturned).Inc()

	case !flow.IsReply:
		// DNS 요청
		h.queries.WithLabels("", qtypes, ipsReturned).Inc()

	case flow.IsReply:
		// DNS 응답
		rcode := dns.RCode
		h.responses.WithLabels(rcode, qtypes, ipsReturned).Inc()

		// 응답 타입별 카운터
		for _, rrtype := range dns.RRTypes {
			h.responseTypes.WithLabels(rrtype, qtypes).Inc()
		}
	}

	return nil
}

func (h *DNSHandler) Status() string { return "dns" }
func (h *DNSHandler) Deinit()        {}

// --- HTTP 메트릭 핸들러 ---
// 실제: http.httpHandler

type HTTPHandler struct {
	requests  *CounterVec
	responses *CounterVec
	duration  *HistogramVec
}

func NewHTTPHandler() *HTTPHandler {
	return &HTTPHandler{}
}

func (h *HTTPHandler) Init() error {
	h.requests = NewCounterVec("hubble_http_requests_total")
	h.responses = NewCounterVec("hubble_http_responses_total")
	// 실제: prometheus.DefBuckets (기본 히스토그램 버킷)
	h.duration = NewHistogramVec("hubble_http_request_duration_seconds",
		[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10})
	return nil
}

// ProcessFlow는 HTTP Flow에서 메트릭을 추출
// 실제: httpHandler.processMetricsV1()
func (h *HTTPHandler) ProcessFlow(ctx context.Context, flow *Flow) error {
	if flow.L7 == nil || flow.L7.HTTP == nil {
		return nil
	}

	http := flow.L7.HTTP
	reporter := "unknown"
	switch flow.TrafficDirection {
	case DirectionEgress:
		reporter = "client"
	case DirectionIngress:
		reporter = "server"
	}

	switch flow.L7.Type {
	case L7Request:
		h.requests.WithLabels(http.Method, http.Protocol, reporter).Inc()

	case L7Response:
		status := fmt.Sprintf("%d", http.Code)
		h.responses.WithLabels(http.Method, http.Protocol, status, reporter).Inc()

		// 지연 시간 히스토그램
		durationSec := float64(http.LatencyNs) / float64(time.Second)
		h.duration.WithLabels(http.Method, reporter).Observe(durationSec)
	}

	return nil
}

func (h *HTTPHandler) Status() string { return "http" }
func (h *HTTPHandler) Deinit()        {}

// --- TCP 메트릭 핸들러 ---

type TCPHandler struct {
	flags *CounterVec
}

func NewTCPHandler() *TCPHandler {
	return &TCPHandler{}
}

func (h *TCPHandler) Init() error {
	h.flags = NewCounterVec("hubble_tcp_flags_total")
	return nil
}

func (h *TCPHandler) ProcessFlow(ctx context.Context, flow *Flow) error {
	if flow.L7 != nil {
		return nil // L7 Flow는 무시
	}
	flag := "SYN"
	if flow.IsReply {
		flag = "SYN-ACK"
	}
	h.flags.WithLabels(flag, string(flow.Verdict)).Inc()
	return nil
}

func (h *TCPHandler) Status() string { return "tcp" }
func (h *TCPHandler) Deinit()        {}

// --- Drop 메트릭 핸들러 ---

type DropHandler struct {
	drops *CounterVec
}

func NewDropHandler() *DropHandler {
	return &DropHandler{}
}

func (h *DropHandler) Init() error {
	h.drops = NewCounterVec("hubble_drop_total")
	return nil
}

func (h *DropHandler) ProcessFlow(ctx context.Context, flow *Flow) error {
	if flow.Verdict != VerdictDropped {
		return nil
	}
	reason := flow.DropReason
	if reason == "" {
		reason = "UNKNOWN"
	}
	h.drops.WithLabels(reason, string(flow.TrafficDirection)).Inc()
	return nil
}

func (h *DropHandler) Status() string { return "drop" }
func (h *DropHandler) Deinit()        {}

// --- NamedHandler ---
// 실제: api.NamedHandler

type NamedHandler struct {
	Name    string
	Handler MetricHandler
}

// --- FlowProcessor ---
// 실제: metrics.StaticFlowProcessor

type FlowProcessor struct {
	handlers []NamedHandler
}

func NewFlowProcessor(handlers []NamedHandler) *FlowProcessor {
	return &FlowProcessor{handlers: handlers}
}

// OnDecodedFlow는 모든 핸들러에 대해 ProcessFlow를 호출
// 실제: StaticFlowProcessor.OnDecodedFlow()
// 하나의 핸들러가 실패해도 나머지 핸들러는 계속 실행
func (p *FlowProcessor) OnDecodedFlow(ctx context.Context, flow *Flow) error {
	var errs []string
	for _, nh := range p.handlers {
		if err := nh.Handler.ProcessFlow(ctx, flow); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", nh.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("ProcessFlow 오류: %s", strings.Join(errs, "; "))
	}
	return nil
}

// --- Flow 생성기 ---

func generateTestFlows() []*Flow {
	var flows []*Flow

	// DNS 요청/응답
	flows = append(flows,
		&Flow{
			Time: time.Now(), Source: "default/frontend-0", Destination: "kube-system/coredns",
			Verdict: VerdictForwarded, TrafficDirection: DirectionEgress, IsReply: false,
			L7: &L7Info{Type: L7Request, DNS: &DNSInfo{Query: "api.example.com.", QTypes: []string{"A"}}},
		},
		&Flow{
			Time: time.Now(), Source: "kube-system/coredns", Destination: "default/frontend-0",
			Verdict: VerdictForwarded, TrafficDirection: DirectionIngress, IsReply: true,
			L7: &L7Info{Type: L7Response, DNS: &DNSInfo{
				Query: "api.example.com.", QTypes: []string{"A"}, RCode: "NOERROR",
				RRTypes: []string{"A"}, IPs: []string{"10.0.1.5", "10.0.1.6"},
			}},
		},
		&Flow{
			Time: time.Now(), Source: "default/backend-0", Destination: "kube-system/coredns",
			Verdict: VerdictDropped, TrafficDirection: DirectionEgress, IsReply: false,
			L7:         &L7Info{Type: L7Request, DNS: &DNSInfo{Query: "blocked.example.com.", QTypes: []string{"A"}}},
			DropReason: "POLICY_DENIED",
		},
	)

	// HTTP 요청/응답
	latencies := []int64{
		5 * int64(time.Millisecond),
		15 * int64(time.Millisecond),
		120 * int64(time.Millisecond),
		500 * int64(time.Millisecond),
	}
	methods := []string{"GET", "POST", "PUT"}
	codes := []int{200, 200, 200, 201, 404, 500}

	for i := 0; i < 20; i++ {
		method := methods[rand.Intn(len(methods))]
		code := codes[rand.Intn(len(codes))]
		latency := latencies[rand.Intn(len(latencies))]

		flows = append(flows,
			&Flow{
				Time: time.Now(), Source: "default/frontend-0", Destination: "default/backend-0",
				Verdict: VerdictForwarded, TrafficDirection: DirectionEgress, IsReply: false,
				L7: &L7Info{Type: L7Request, HTTP: &HTTPInfo{
					Method: method, URL: "/api/v1/data", Protocol: "HTTP/1.1",
				}},
			},
			&Flow{
				Time: time.Now(), Source: "default/backend-0", Destination: "default/frontend-0",
				Verdict: VerdictForwarded, TrafficDirection: DirectionIngress, IsReply: true,
				L7: &L7Info{Type: L7Response, HTTP: &HTTPInfo{
					Method: method, URL: "/api/v1/data", Protocol: "HTTP/1.1",
					Code: code, LatencyNs: latency,
				}},
			},
		)
	}

	// TCP Flow (L4)
	for i := 0; i < 15; i++ {
		verdict := VerdictForwarded
		dropReason := ""
		if rand.Float32() < 0.2 {
			verdict = VerdictDropped
			reasons := []string{"POLICY_DENIED", "CT_TRUNCATED", "CT_MAP_FULL"}
			dropReason = reasons[rand.Intn(len(reasons))]
		}
		flows = append(flows, &Flow{
			Time: time.Now(), Source: "prod/service-a", Destination: "prod/service-b",
			Verdict: verdict, TrafficDirection: DirectionEgress,
			IsReply: rand.Float32() < 0.5, DropReason: dropReason,
		})
	}

	return flows
}

func main() {
	fmt.Println("=== Hubble 메트릭 핸들러 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: cilium/pkg/hubble/metrics/flow_processor.go - StaticFlowProcessor")
	fmt.Println("참조: cilium/pkg/hubble/metrics/api/api.go        - Handler 인터페이스")
	fmt.Println("참조: cilium/pkg/hubble/metrics/dns/handler.go    - dnsHandler.ProcessFlow()")
	fmt.Println("참조: cilium/pkg/hubble/metrics/http/handler.go   - httpHandler.ProcessFlow()")
	fmt.Println()

	// 핸들러 초기화
	// 실제: 각 핸들러의 Init() → prometheus.Registry에 메트릭 등록
	dnsHandler := NewDNSHandler()
	httpHandler := NewHTTPHandler()
	tcpHandler := NewTCPHandler()
	dropHandler := NewDropHandler()

	handlers := []NamedHandler{
		{Name: "dns", Handler: dnsHandler},
		{Name: "http", Handler: httpHandler},
		{Name: "tcp", Handler: tcpHandler},
		{Name: "drop", Handler: dropHandler},
	}

	for _, nh := range handlers {
		if err := nh.Handler.Init(); err != nil {
			fmt.Printf("핸들러 초기화 실패 [%s]: %v\n", nh.Name, err)
			return
		}
		fmt.Printf("핸들러 등록: %s (status: %s)\n", nh.Name, nh.Handler.Status())
	}
	fmt.Println()

	// FlowProcessor 생성
	processor := NewFlowProcessor(handlers)

	// 테스트 Flow 생성 및 처리
	flows := generateTestFlows()
	fmt.Printf("총 %d개 Flow 처리 시작...\n\n", len(flows))

	ctx := context.Background()
	for _, flow := range flows {
		if err := processor.OnDecodedFlow(ctx, flow); err != nil {
			fmt.Printf("처리 오류: %v\n", err)
		}
	}

	// 메트릭 출력 (Prometheus /metrics 엔드포인트 시뮬레이션)
	fmt.Println("=== 메트릭 결과 (Prometheus 형식 시뮬레이션) ===")
	fmt.Println()

	fmt.Println("[DNS 메트릭]")
	dnsHandler.queries.Print()
	dnsHandler.responses.Print()
	dnsHandler.responseTypes.Print()
	fmt.Println()

	fmt.Println("[HTTP 메트릭]")
	httpHandler.requests.Print()
	httpHandler.responses.Print()
	httpHandler.duration.Print()
	fmt.Println()

	fmt.Println("[TCP 메트릭]")
	tcpHandler.flags.Print()
	fmt.Println()

	fmt.Println("[Drop 메트릭]")
	dropHandler.drops.Print()
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. Handler 인터페이스: Init/ProcessFlow/Status/Deinit 표준화")
	fmt.Println("  2. FlowProcessor: 모든 핸들러를 순회하며 ProcessFlow 호출")
	fmt.Println("     - 하나의 핸들러 실패가 다른 핸들러에 영향 없음 (errors.Join)")
	fmt.Println("  3. 프로토콜별 메트릭 추출:")
	fmt.Println("     - DNS: queries_total, responses_total, response_types_total")
	fmt.Println("     - HTTP: requests_total, responses_total, request_duration_seconds")
	fmt.Println("     - Drop: drop_total (reason별 분류)")
	fmt.Println("  4. ContextOptions: source/destination 라벨을 메트릭에 추가")
}
