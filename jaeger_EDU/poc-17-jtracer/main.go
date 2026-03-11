package main

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Jaeger Self-Tracing (jtracer) 시뮬레이션
// =============================================================================
//
// Jaeger는 자기 자신도 추적(self-tracing)하여 내부 동작을 관측한다.
// jtracer 패키지는 OTel SDK를 부트스트랩하여 Jaeger 자체의 span을 생성한다.
//
// 핵심 개념:
//   - Bootstrap: OTel SDK 초기화 (TracerProvider, Exporter 설정)
//   - Self-tracing: Jaeger 내부 처리 과정 자체를 trace
//   - Shutdown: 종료 시 버퍼된 span을 flush
//
// 실제 코드 참조:
//   - cmd/jaeger/internal/jtracer/: jtracer 패키지
// =============================================================================

// --- Span 모델 ---

type TraceID [16]byte
type SpanID [8]byte

func (t TraceID) String() string { return fmt.Sprintf("%x", t[:8]) }
func (s SpanID) String() string  { return fmt.Sprintf("%x", s[:4]) }

type Span struct {
	TraceID    TraceID
	SpanID     SpanID
	ParentID   SpanID
	Name       string
	Service    string
	StartTime  time.Time
	EndTime    time.Time
	Attributes map[string]string
	Events     []SpanEvent
	Status     string
}

type SpanEvent struct {
	Name      string
	Timestamp time.Time
}

// --- Exporter ---

type SpanExporter interface {
	Export(spans []Span) error
	Shutdown() error
}

type InMemoryExporter struct {
	mu    sync.Mutex
	spans []Span
}

func (e *InMemoryExporter) Export(spans []Span) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *InMemoryExporter) Shutdown() error {
	fmt.Println("    [Exporter] Shutdown - flushing remaining spans")
	return nil
}

func (e *InMemoryExporter) GetSpans() []Span {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Span{}, e.spans...)
}

// --- TracerProvider ---

type TracerProvider struct {
	serviceName string
	exporter    SpanExporter
	sampler     Sampler
	buffer      []Span
	mu          sync.Mutex
	batchSize   int
	r           *rand.Rand
}

type Sampler struct {
	Ratio float64
}

func (s Sampler) ShouldSample() bool {
	return rand.Float64() < s.Ratio
}

func NewTracerProvider(serviceName string, exporter SpanExporter, sampleRatio float64) *TracerProvider {
	return &TracerProvider{
		serviceName: serviceName,
		exporter:    exporter,
		sampler:     Sampler{Ratio: sampleRatio},
		batchSize:   5,
		r:           rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (tp *TracerProvider) Tracer(name string) *Tracer {
	return &Tracer{provider: tp, name: name}
}

func (tp *TracerProvider) recordSpan(span Span) {
	if !tp.sampler.ShouldSample() {
		return
	}
	tp.mu.Lock()
	tp.buffer = append(tp.buffer, span)
	shouldFlush := len(tp.buffer) >= tp.batchSize
	var toExport []Span
	if shouldFlush {
		toExport = tp.buffer
		tp.buffer = nil
	}
	tp.mu.Unlock()

	if shouldFlush && len(toExport) > 0 {
		tp.exporter.Export(toExport)
	}
}

func (tp *TracerProvider) Shutdown() error {
	tp.mu.Lock()
	remaining := tp.buffer
	tp.buffer = nil
	tp.mu.Unlock()

	if len(remaining) > 0 {
		tp.exporter.Export(remaining)
	}
	return tp.exporter.Shutdown()
}

func (tp *TracerProvider) newTraceID() TraceID {
	var id TraceID
	for i := range id {
		id[i] = byte(tp.r.Intn(256))
	}
	return id
}

func (tp *TracerProvider) newSpanID() SpanID {
	var id SpanID
	for i := range id {
		id[i] = byte(tp.r.Intn(256))
	}
	return id
}

// --- Tracer ---

type Tracer struct {
	provider *TracerProvider
	name     string
}

type spanContextKey struct{}

type SpanContext struct {
	TraceID TraceID
	SpanID  SpanID
}

func (t *Tracer) Start(ctx context.Context, name string) (context.Context, *ActiveSpan) {
	span := &ActiveSpan{
		span: Span{
			SpanID:     t.provider.newSpanID(),
			Name:       name,
			Service:    t.provider.serviceName,
			StartTime:  time.Now(),
			Attributes: make(map[string]string),
			Status:     "OK",
		},
		tracer: t,
	}

	if parentSC, ok := ctx.Value(spanContextKey{}).(SpanContext); ok {
		span.span.TraceID = parentSC.TraceID
		span.span.ParentID = parentSC.SpanID
	} else {
		span.span.TraceID = t.provider.newTraceID()
	}

	newCtx := context.WithValue(ctx, spanContextKey{}, SpanContext{
		TraceID: span.span.TraceID,
		SpanID:  span.span.SpanID,
	})

	return newCtx, span
}

type ActiveSpan struct {
	span   Span
	tracer *Tracer
}

func (s *ActiveSpan) SetAttribute(key, value string) {
	s.span.Attributes[key] = value
}

func (s *ActiveSpan) AddEvent(name string) {
	s.span.Events = append(s.span.Events, SpanEvent{Name: name, Timestamp: time.Now()})
}

func (s *ActiveSpan) SetError(err error) {
	s.span.Status = "ERROR"
	s.span.Attributes["error.message"] = err.Error()
}

func (s *ActiveSpan) End() {
	s.span.EndTime = time.Now()
	s.tracer.provider.recordSpan(s.span)
}

// --- JTracer (Self-Tracing Bootstrap) ---

type JTracer struct {
	provider *TracerProvider
	exporter *InMemoryExporter
	closed   bool
}

// NewJTracer는 Jaeger의 self-tracing을 초기화한다.
func NewJTracer(serviceName string) *JTracer {
	exporter := &InMemoryExporter{}
	provider := NewTracerProvider(serviceName, exporter, 1.0) // 100% 샘플링

	fmt.Printf("  [JTracer] Bootstrap OTel SDK:\n")
	fmt.Printf("    Service: %s\n", serviceName)
	fmt.Printf("    Exporter: InMemory\n")
	fmt.Printf("    Sampler: AlwaysOn (ratio=1.0)\n")
	fmt.Printf("    BatchSize: %d\n", provider.batchSize)

	return &JTracer{
		provider: provider,
		exporter: exporter,
	}
}

func (jt *JTracer) Tracer(name string) *Tracer {
	return jt.provider.Tracer(name)
}

func (jt *JTracer) Shutdown() {
	if jt.closed {
		return
	}
	jt.closed = true
	fmt.Println("  [JTracer] Shutdown initiated")
	jt.provider.Shutdown()
	fmt.Printf("  [JTracer] Total spans exported: %d\n", len(jt.exporter.GetSpans()))
}

func main() {
	fmt.Println("=== Jaeger Self-Tracing (jtracer) 시뮬레이션 ===")
	fmt.Println()

	// --- JTracer 부트스트랩 ---
	fmt.Println("[1] JTracer 부트스트랩")
	fmt.Println(strings.Repeat("-", 60))

	jt := NewJTracer("jaeger-all-in-one")
	fmt.Println()

	// --- Jaeger 내부 처리 self-tracing ---
	fmt.Println("[2] Jaeger 내부 처리 self-tracing")
	fmt.Println(strings.Repeat("-", 60))

	tracer := jt.Tracer("jaeger-collector")

	// Span 수신 처리
	for i := 0; i < 5; i++ {
		ctx := context.Background()
		ctx, receiveSpan := tracer.Start(ctx, "collector.ReceiveSpans")
		receiveSpan.SetAttribute("batch.size", fmt.Sprintf("%d", 10+i*5))
		receiveSpan.SetAttribute("protocol", "OTLP/gRPC")
		receiveSpan.AddEvent("batch_received")

		// 내부: 검증
		_, validateSpan := tracer.Start(ctx, "collector.ValidateSpans")
		validateSpan.SetAttribute("valid_count", fmt.Sprintf("%d", 10+i*5))
		time.Sleep(time.Millisecond)
		validateSpan.End()

		// 내부: 저장
		_, storeSpan := tracer.Start(ctx, "collector.StoreSpans")
		storeSpan.SetAttribute("storage.backend", "elasticsearch")
		time.Sleep(2 * time.Millisecond)
		storeSpan.AddEvent("spans_written")
		storeSpan.End()

		receiveSpan.AddEvent("batch_processed")
		time.Sleep(time.Millisecond)
		receiveSpan.End()
	}

	// 쿼리 처리
	queryTracer := jt.Tracer("jaeger-query")
	for i := 0; i < 3; i++ {
		ctx := context.Background()
		ctx, querySpan := queryTracer.Start(ctx, "query.FindTraces")
		querySpan.SetAttribute("service", "frontend")
		querySpan.SetAttribute("duration", "1h")

		_, searchSpan := queryTracer.Start(ctx, "query.SearchIndex")
		searchSpan.SetAttribute("index", "jaeger-span-*")
		time.Sleep(time.Millisecond)
		searchSpan.End()

		time.Sleep(time.Millisecond)
		querySpan.SetAttribute("result.count", fmt.Sprintf("%d", 5+i))
		querySpan.End()
	}

	// 에러 케이스
	ctx := context.Background()
	_, errorSpan := tracer.Start(ctx, "collector.ReceiveSpans")
	errorSpan.SetError(fmt.Errorf("storage unavailable"))
	errorSpan.End()
	fmt.Println("  Self-tracing 완료")
	fmt.Println()

	// --- 수집된 Span 출력 ---
	fmt.Println("[3] 수집된 Self-Trace Spans")
	fmt.Println(strings.Repeat("-", 60))

	// Shutdown으로 남은 버퍼 flush
	jt.Shutdown()
	fmt.Println()

	spans := jt.exporter.GetSpans()
	for _, span := range spans {
		parent := "root"
		if span.ParentID != (SpanID{}) {
			parent = span.ParentID.String()
		}
		indent := ""
		if parent != "root" {
			indent = "  "
		}
		dur := span.EndTime.Sub(span.StartTime)
		fmt.Printf("  %s[%s] %s (trace=%s span=%s parent=%s dur=%s status=%s)\n",
			indent, span.Service, span.Name, span.TraceID, span.SpanID, parent, dur, span.Status)
		for k, v := range span.Attributes {
			fmt.Printf("  %s  %s=%s\n", indent, k, v)
		}
		for _, evt := range span.Events {
			fmt.Printf("  %s  EVENT: %s\n", indent, evt.Name)
		}
	}
	fmt.Println()

	// --- 통계 ---
	fmt.Println("[4] Self-Trace 통계")
	fmt.Println(strings.Repeat("-", 60))
	serviceCount := make(map[string]int)
	errorCount := 0
	for _, span := range spans {
		serviceCount[span.Service]++
		if span.Status == "ERROR" {
			errorCount++
		}
	}
	fmt.Printf("  Total spans: %d\n", len(spans))
	for svc, count := range serviceCount {
		fmt.Printf("  Service %-25s: %d spans\n", svc, count)
	}
	fmt.Printf("  Error spans: %d\n", errorCount)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
