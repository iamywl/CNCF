package main

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"
)

// =============================================================================
// gRPC OpenTelemetry 통합 시뮬레이션
// =============================================================================
//
// gRPC는 OpenTelemetry(OTel)와 통합하여 분산 추적, 메트릭스, 컨텍스트 전파를 수행한다.
// 핵심: interceptor를 통해 자동으로 span 생성 및 trace context 전파.
//
// 실제 코드 참조:
//   - stats/opentelemetry/: OTel stats 핸들러
//   - metadata/: 컨텍스트 전파 (traceparent 헤더)
// =============================================================================

// --- Trace Context (W3C Trace Context) ---

type TraceID [16]byte
type SpanID [8]byte

func newTraceID(r *rand.Rand) TraceID {
	var id TraceID
	for i := range id {
		id[i] = byte(r.Intn(256))
	}
	return id
}

func newSpanID(r *rand.Rand) SpanID {
	var id SpanID
	for i := range id {
		id[i] = byte(r.Intn(256))
	}
	return id
}

func (t TraceID) String() string {
	return fmt.Sprintf("%x", [16]byte(t))
}

func (s SpanID) String() string {
	return fmt.Sprintf("%x", [8]byte(s))
}

// TraceparentHeader는 W3C traceparent 헤더를 생성한다.
func TraceparentHeader(traceID TraceID, spanID SpanID, sampled bool) string {
	flags := "00"
	if sampled {
		flags = "01"
	}
	return fmt.Sprintf("00-%s-%s-%s", traceID, spanID, flags)
}

// --- Span ---

type SpanKind int

const (
	SpanKindClient SpanKind = iota
	SpanKindServer
	SpanKindInternal
)

func (k SpanKind) String() string {
	return []string{"CLIENT", "SERVER", "INTERNAL"}[k]
}

type SpanStatus int

const (
	StatusOK SpanStatus = iota
	StatusError
)

type SpanEvent struct {
	Name      string
	Timestamp time.Time
	Attrs     map[string]string
}

type Span struct {
	TraceID    TraceID
	SpanID     SpanID
	ParentID   SpanID
	Name       string
	Kind       SpanKind
	StartTime  time.Time
	EndTime    time.Time
	Status     SpanStatus
	Attributes map[string]string
	Events     []SpanEvent
}

func (s Span) Duration() time.Duration {
	return s.EndTime.Sub(s.StartTime)
}

func (s Span) String() string {
	parent := "root"
	if s.ParentID != (SpanID{}) {
		parent = s.ParentID.String()[:8]
	}
	status := "OK"
	if s.Status == StatusError {
		status = "ERROR"
	}
	return fmt.Sprintf("[%s] %s (trace=%s span=%s parent=%s dur=%s status=%s)",
		s.Kind, s.Name, s.TraceID.String()[:8], s.SpanID.String()[:8], parent, s.Duration(), status)
}

// --- Span Context (context.Context에 저장) ---

type spanContextKey struct{}

type SpanContext struct {
	TraceID TraceID
	SpanID  SpanID
	Sampled bool
}

func contextWithSpan(ctx context.Context, sc SpanContext) context.Context {
	return context.WithValue(ctx, spanContextKey{}, sc)
}

func spanFromContext(ctx context.Context) (SpanContext, bool) {
	sc, ok := ctx.Value(spanContextKey{}).(SpanContext)
	return sc, ok
}

// --- Tracer ---

type Tracer struct {
	spans  []Span
	r      *rand.Rand
	nextID uint64
}

func NewTracer() *Tracer {
	return &Tracer{
		r: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (t *Tracer) StartSpan(ctx context.Context, name string, kind SpanKind) (context.Context, *Span) {
	span := &Span{
		SpanID:     newSpanID(t.r),
		Name:       name,
		Kind:       kind,
		StartTime:  time.Now(),
		Attributes: make(map[string]string),
	}

	// 부모 컨텍스트에서 trace/span 상속
	if parentSC, ok := spanFromContext(ctx); ok {
		span.TraceID = parentSC.TraceID
		span.ParentID = parentSC.SpanID
	} else {
		span.TraceID = newTraceID(t.r)
	}

	newSC := SpanContext{
		TraceID: span.TraceID,
		SpanID:  span.SpanID,
		Sampled: true,
	}

	return contextWithSpan(ctx, newSC), span
}

func (t *Tracer) EndSpan(span *Span) {
	span.EndTime = time.Now()
	t.spans = append(t.spans, *span)
}

// --- gRPC 메트릭스 ---

type RPCMetrics struct {
	totalRPCs         int64
	totalErrors       int64
	totalLatencyNs    int64
	sentMessages      int64
	receivedMessages  int64
	sentBytes         int64
	receivedBytes     int64
}

var metrics RPCMetrics

func recordRPCStart() {
	atomic.AddInt64(&metrics.totalRPCs, 1)
}

func recordRPCEnd(latency time.Duration, err bool) {
	atomic.AddInt64(&metrics.totalLatencyNs, int64(latency))
	if err {
		atomic.AddInt64(&metrics.totalErrors, 1)
	}
}

func recordMessage(sent bool, bytes int64) {
	if sent {
		atomic.AddInt64(&metrics.sentMessages, 1)
		atomic.AddInt64(&metrics.sentBytes, bytes)
	} else {
		atomic.AddInt64(&metrics.receivedMessages, 1)
		atomic.AddInt64(&metrics.receivedBytes, bytes)
	}
}

// --- gRPC Interceptor 시뮬레이션 ---

type RPCHandler func(ctx context.Context, method string, req interface{}) (interface{}, error)

// UnaryClientInterceptor는 클라이언트 unary interceptor를 시뮬레이션한다.
func UnaryClientInterceptor(tracer *Tracer) func(ctx context.Context, method string, handler RPCHandler, req interface{}) (interface{}, error) {
	return func(ctx context.Context, method string, handler RPCHandler, req interface{}) (interface{}, error) {
		ctx, span := tracer.StartSpan(ctx, method, SpanKindClient)
		span.Attributes["rpc.system"] = "grpc"
		span.Attributes["rpc.method"] = method
		span.Attributes["rpc.service"] = strings.Split(strings.TrimPrefix(method, "/"), "/")[0]

		recordRPCStart()
		recordMessage(true, 64)

		// traceparent 헤더 주입 (metadata 전파)
		sc, _ := spanFromContext(ctx)
		traceparent := TraceparentHeader(sc.TraceID, sc.SpanID, sc.Sampled)
		span.Attributes["traceparent"] = traceparent

		resp, err := handler(ctx, method, req)

		if err != nil {
			span.Status = StatusError
			span.Events = append(span.Events, SpanEvent{
				Name:      "exception",
				Timestamp: time.Now(),
				Attrs:     map[string]string{"message": fmt.Sprintf("%v", err)},
			})
			recordRPCEnd(time.Since(span.StartTime), true)
		} else {
			span.Status = StatusOK
			recordMessage(false, 128)
			recordRPCEnd(time.Since(span.StartTime), false)
		}

		tracer.EndSpan(span)
		return resp, err
	}
}

// UnaryServerInterceptor는 서버 unary interceptor를 시뮬레이션한다.
func UnaryServerInterceptor(tracer *Tracer) func(ctx context.Context, method string, handler RPCHandler, req interface{}) (interface{}, error) {
	return func(ctx context.Context, method string, handler RPCHandler, req interface{}) (interface{}, error) {
		ctx, span := tracer.StartSpan(ctx, method, SpanKindServer)
		span.Attributes["rpc.system"] = "grpc"
		span.Attributes["rpc.method"] = method

		resp, err := handler(ctx, method, req)

		if err != nil {
			span.Status = StatusError
		}
		tracer.EndSpan(span)
		return resp, err
	}
}

func main() {
	fmt.Println("=== gRPC OpenTelemetry 통합 시뮬레이션 ===")
	fmt.Println()

	tracer := NewTracer()
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	clientInterceptor := UnaryClientInterceptor(tracer)
	serverInterceptor := UnaryServerInterceptor(tracer)

	// --- RPC 핸들러 시뮬레이션 ---
	sayHelloHandler := func(ctx context.Context, method string, req interface{}) (interface{}, error) {
		// 서버에서 내부 span 생성 (DB 조회 등)
		_, innerSpan := tracer.StartSpan(ctx, "db.query", SpanKindInternal)
		innerSpan.Attributes["db.system"] = "postgresql"
		innerSpan.Attributes["db.statement"] = "SELECT name FROM users WHERE id = ?"
		time.Sleep(time.Duration(r.Intn(5)) * time.Millisecond)
		tracer.EndSpan(innerSpan)

		return map[string]string{"message": "Hello!"}, nil
	}

	errorHandler := func(ctx context.Context, method string, req interface{}) (interface{}, error) {
		return nil, fmt.Errorf("service unavailable")
	}

	// --- RPC 호출 시뮬레이션 ---
	fmt.Println("[1] RPC 호출 + 분산 추적")
	fmt.Println(strings.Repeat("-", 60))

	rpcs := []struct {
		method  string
		handler RPCHandler
		desc    string
	}{
		{"/helloworld.Greeter/SayHello", sayHelloHandler, "성공 RPC"},
		{"/helloworld.Greeter/SayHello", sayHelloHandler, "성공 RPC"},
		{"/routeguide.RouteGuide/GetFeature", sayHelloHandler, "성공 RPC"},
		{"/helloworld.Greeter/SayHello", errorHandler, "실패 RPC"},
		{"/helloworld.Greeter/SayHello", sayHelloHandler, "성공 RPC"},
	}

	for _, rpc := range rpcs {
		ctx := context.Background()
		fmt.Printf("\n  >> %s: %s\n", rpc.desc, rpc.method)

		// Client interceptor -> Server interceptor -> Handler
		wrappedHandler := func(ctx context.Context, method string, req interface{}) (interface{}, error) {
			return serverInterceptor(ctx, method, rpc.handler, req)
		}

		resp, err := clientInterceptor(ctx, rpc.method, wrappedHandler, nil)
		if err != nil {
			fmt.Printf("     Error: %v\n", err)
		} else {
			fmt.Printf("     Response: %v\n", resp)
		}
	}
	fmt.Println()

	// --- 수집된 Span 출력 ---
	fmt.Println("[2] 수집된 Trace Spans")
	fmt.Println(strings.Repeat("-", 60))

	for _, span := range tracer.spans {
		fmt.Printf("  %s\n", span)
		if len(span.Events) > 0 {
			for _, evt := range span.Events {
				fmt.Printf("    EVENT: %s %v\n", evt.Name, evt.Attrs)
			}
		}
	}
	fmt.Println()

	// --- Trace 그래프 ---
	fmt.Println("[3] Trace 시각화 (ASCII)")
	fmt.Println(strings.Repeat("-", 60))

	// traceID별 그룹화
	traceGroups := make(map[string][]Span)
	for _, span := range tracer.spans {
		key := span.TraceID.String()[:8]
		traceGroups[key] = append(traceGroups[key], span)
	}

	for traceID, spans := range traceGroups {
		fmt.Printf("\n  Trace: %s... (%d spans)\n", traceID, len(spans))
		for _, span := range spans {
			indent := "  "
			if span.Kind == SpanKindServer {
				indent = "    "
			} else if span.Kind == SpanKindInternal {
				indent = "      "
			}
			bar := strings.Repeat("=", int(span.Duration().Milliseconds())+1)
			if len(bar) > 30 {
				bar = bar[:30]
			}
			fmt.Printf("  %s|%s| %s [%s] %s\n", indent, bar, span.Name, span.Kind, span.Duration())
		}
	}
	fmt.Println()

	// --- W3C Trace Context 전파 ---
	fmt.Println("[4] W3C Trace Context (traceparent 헤더)")
	fmt.Println(strings.Repeat("-", 60))

	for _, span := range tracer.spans {
		if tp, ok := span.Attributes["traceparent"]; ok {
			fmt.Printf("  %s -> traceparent: %s\n", span.Name, tp)
		}
	}
	fmt.Println()

	// --- 메트릭스 ---
	fmt.Println("[5] gRPC OTel 메트릭스")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("  grpc.client.attempt.started:    %d\n", atomic.LoadInt64(&metrics.totalRPCs))
	fmt.Printf("  grpc.client.attempt.duration:   %s (total)\n",
		time.Duration(atomic.LoadInt64(&metrics.totalLatencyNs)))
	fmt.Printf("  grpc.client.attempt.errors:     %d\n", atomic.LoadInt64(&metrics.totalErrors))
	fmt.Printf("  grpc.client.sent_messages:      %d\n", atomic.LoadInt64(&metrics.sentMessages))
	fmt.Printf("  grpc.client.rcvd_messages:      %d\n", atomic.LoadInt64(&metrics.receivedMessages))
	fmt.Printf("  grpc.client.sent_bytes:         %d\n", atomic.LoadInt64(&metrics.sentBytes))
	fmt.Printf("  grpc.client.rcvd_bytes:         %d\n", atomic.LoadInt64(&metrics.receivedBytes))
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
