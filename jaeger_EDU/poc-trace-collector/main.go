package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Jaeger Trace Collection Pipeline 시뮬레이션
// =============================================================================
// Jaeger v2는 OpenTelemetry Collector 기반으로 재구축되었다.
// 수집 파이프라인은 Receiver → Processor → Exporter 3단계로 구성된다.
//
// 실제 Jaeger 소스에서의 대응:
// - Receiver: OTLP/Jaeger/Zipkin 프로토콜로 Span을 수신
// - Processor: 배치 처리, 샘플링, 태그 수정 등
// - Exporter: 스토리지(Cassandra, ES, Memory 등)에 기록
//
// 이 PoC에서는 이 파이프라인 구조를 단순화하여 재현하고,
// 특히 BatchProcessor의 배치 크기 + 플러시 간격 기반 동작을 시뮬레이션한다.
// =============================================================================

// =============================================================================
// 데이터 모델 (간소화된 Span)
// =============================================================================

// Span은 파이프라인을 통해 전달되는 기본 데이터 단위이다.
type Span struct {
	TraceID       string            `json:"traceID"`
	SpanID        string            `json:"spanID"`
	OperationName string            `json:"operationName"`
	ServiceName   string            `json:"serviceName"`
	StartTime     time.Time         `json:"startTime"`
	Duration      time.Duration     `json:"duration"`
	Tags          map[string]string `json:"tags,omitempty"`
}

func (s Span) String() string {
	return fmt.Sprintf("[%s] %s::%s (%s)",
		s.SpanID[:8], s.ServiceName, s.OperationName, s.Duration)
}

// =============================================================================
// 파이프라인 인터페이스
// =============================================================================

// Receiver는 외부에서 Span을 수신하는 컴포넌트이다.
// 실제 Jaeger에서는 OTLP, Jaeger Thrift, Zipkin 등 다양한 프로토콜을 지원한다.
type Receiver interface {
	// Start는 수신기를 시작하고 수신된 Span을 채널로 전달한다.
	Start(output chan<- Span)
	// Stop은 수신기를 중지한다.
	Stop()
	// Name은 수신기 이름을 반환한다.
	Name() string
}

// Processor는 수신된 Span을 가공하는 컴포넌트이다.
// 배치 처리, 필터링, 태그 수정, 샘플링 등을 수행한다.
type Processor interface {
	// Start는 프로세서를 시작한다.
	Start(input <-chan Span, output chan<- []Span)
	// Stop은 프로세서를 중지하고 남은 배치를 플러시한다.
	Stop()
	// Name은 프로세서 이름을 반환한다.
	Name() string
}

// Exporter는 처리된 Span을 저장소에 기록하는 컴포넌트이다.
// 실제 Jaeger에서는 Cassandra, Elasticsearch, Kafka, Memory 등을 지원한다.
type Exporter interface {
	// Start는 내보내기를 시작한다.
	Start(input <-chan []Span)
	// Stop은 내보내기를 중지한다.
	Stop()
	// Name은 내보내기 이름을 반환한다.
	Name() string
}

// =============================================================================
// 메트릭 수집기
// =============================================================================

// PipelineMetrics는 파이프라인 각 단계의 처리 통계를 추적한다.
type PipelineMetrics struct {
	ReceivedCount  atomic.Int64
	ProcessedCount atomic.Int64
	ExportedCount  atomic.Int64
	DroppedCount   atomic.Int64
	BatchCount     atomic.Int64
	FlushCount     atomic.Int64 // 타이머 기반 플러시 횟수
	BatchFlush     atomic.Int64 // 배치 크기 기반 플러시 횟수
}

func (m *PipelineMetrics) Print() {
	fmt.Printf("  수신(Received):     %d spans\n", m.ReceivedCount.Load())
	fmt.Printf("  처리(Processed):    %d spans\n", m.ProcessedCount.Load())
	fmt.Printf("  내보내기(Exported): %d spans\n", m.ExportedCount.Load())
	fmt.Printf("  드롭(Dropped):      %d spans\n", m.DroppedCount.Load())
	fmt.Printf("  배치 총 횟수:       %d batches\n", m.BatchCount.Load())
	fmt.Printf("    - 크기 기반 플러시: %d회\n", m.BatchFlush.Load())
	fmt.Printf("    - 타이머 기반 플러시: %d회\n", m.FlushCount.Load())
}

// =============================================================================
// HTTP Receiver 구현
// =============================================================================

// HTTPReceiver는 JSON 형식의 Span을 수신하는 HTTP 수신기를 시뮬레이션한다.
// 실제 Jaeger에서는 OTLP gRPC/HTTP, Jaeger Thrift, Zipkin 등을 지원한다.
type HTTPReceiver struct {
	metrics  *PipelineMetrics
	stopCh   chan struct{}
	spans    []Span // 시뮬레이션할 Span 목록
	interval time.Duration
}

func NewHTTPReceiver(metrics *PipelineMetrics, spans []Span, interval time.Duration) *HTTPReceiver {
	return &HTTPReceiver{
		metrics:  metrics,
		stopCh:   make(chan struct{}),
		spans:    spans,
		interval: interval,
	}
}

func (r *HTTPReceiver) Name() string { return "http-receiver" }

func (r *HTTPReceiver) Start(output chan<- Span) {
	go func() {
		for _, span := range r.spans {
			select {
			case <-r.stopCh:
				return
			default:
				// HTTP 요청 수신을 시뮬레이션
				jsonData, _ := json.Marshal(span)
				fmt.Printf("  [%s] 수신: %s (JSON %d bytes)\n",
					r.Name(), span, len(jsonData))

				r.metrics.ReceivedCount.Add(1)
				output <- span

				// 실제 시스템에서 요청이 간헐적으로 도착하는 것을 시뮬레이션
				time.Sleep(r.interval)
			}
		}
	}()
}

func (r *HTTPReceiver) Stop() {
	close(r.stopCh)
}

// =============================================================================
// Batch Processor 구현
// =============================================================================

// BatchProcessor는 Span을 배치로 모아서 처리하는 프로세서이다.
// OpenTelemetry Collector의 batchprocessor와 동일한 개념이다.
//
// 두 가지 조건 중 하나라도 충족되면 배치를 플러시한다:
// 1. 배치 크기가 maxBatchSize에 도달
// 2. flushInterval 시간이 경과 (배치가 비어있지 않을 때)
//
// 이는 처리량(throughput)과 지연(latency) 사이의 균형을 맞추는 핵심 메커니즘이다.
type BatchProcessor struct {
	maxBatchSize  int
	flushInterval time.Duration
	metrics       *PipelineMetrics
	stopCh        chan struct{}
	done          chan struct{}
}

func NewBatchProcessor(maxBatchSize int, flushInterval time.Duration, metrics *PipelineMetrics) *BatchProcessor {
	return &BatchProcessor{
		maxBatchSize:  maxBatchSize,
		flushInterval: flushInterval,
		metrics:       metrics,
		stopCh:        make(chan struct{}),
		done:          make(chan struct{}),
	}
}

func (p *BatchProcessor) Name() string { return "batch-processor" }

func (p *BatchProcessor) Start(input <-chan Span, output chan<- []Span) {
	go func() {
		defer close(p.done)

		batch := make([]Span, 0, p.maxBatchSize)
		timer := time.NewTimer(p.flushInterval)
		defer timer.Stop()

		flush := func(reason string) {
			if len(batch) == 0 {
				return
			}
			// 배치 복사본을 전달 (원본 슬라이스 재사용을 위해)
			batchCopy := make([]Span, len(batch))
			copy(batchCopy, batch)

			p.metrics.ProcessedCount.Add(int64(len(batchCopy)))
			p.metrics.BatchCount.Add(1)

			if reason == "size" {
				p.metrics.BatchFlush.Add(1)
			} else {
				p.metrics.FlushCount.Add(1)
			}

			fmt.Printf("  [%s] 배치 플러시 (%s): %d spans\n",
				p.Name(), reason, len(batchCopy))

			output <- batchCopy
			batch = batch[:0] // 슬라이스 재사용
			timer.Reset(p.flushInterval)
		}

		for {
			select {
			case span, ok := <-input:
				if !ok {
					// 입력 채널이 닫힘 → 남은 배치 플러시
					flush("channel-closed")
					return
				}
				batch = append(batch, span)
				if len(batch) >= p.maxBatchSize {
					flush("size")
				}
			case <-timer.C:
				flush("timer")
				timer.Reset(p.flushInterval)
			case <-p.stopCh:
				// 중지 시 남은 배치 플러시
				flush("shutdown")
				return
			}
		}
	}()
}

func (p *BatchProcessor) Stop() {
	close(p.stopCh)
	<-p.done // 플러시 완료 대기
}

// =============================================================================
// Storage Exporter 구현
// =============================================================================

// StorageExporter는 배치된 Span을 인메모리 저장소에 기록하는 내보내기이다.
// 실제 Jaeger의 internal/storage/v2/memory 패키지를 단순화한 것이다.
type StorageExporter struct {
	metrics *PipelineMetrics
	mu      sync.RWMutex
	traces  map[string][]Span // traceID → spans
	stopCh  chan struct{}
	done    chan struct{}
}

func NewStorageExporter(metrics *PipelineMetrics) *StorageExporter {
	return &StorageExporter{
		metrics: metrics,
		traces:  make(map[string][]Span),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (e *StorageExporter) Name() string { return "storage-exporter" }

func (e *StorageExporter) Start(input <-chan []Span) {
	go func() {
		defer close(e.done)
		for {
			select {
			case batch, ok := <-input:
				if !ok {
					return
				}
				e.writeBatch(batch)
			case <-e.stopCh:
				// 중지 전 남은 배치 처리
				for {
					select {
					case batch := <-input:
						e.writeBatch(batch)
					default:
						return
					}
				}
			}
		}
	}()
}

func (e *StorageExporter) writeBatch(batch []Span) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, span := range batch {
		e.traces[span.TraceID] = append(e.traces[span.TraceID], span)
		e.metrics.ExportedCount.Add(1)
	}
	fmt.Printf("  [%s] 저장 완료: %d spans (현재 총 %d traces)\n",
		e.Name(), len(batch), len(e.traces))
}

func (e *StorageExporter) Stop() {
	close(e.stopCh)
	<-e.done
}

func (e *StorageExporter) GetTraces() map[string][]Span {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make(map[string][]Span, len(e.traces))
	for k, v := range e.traces {
		result[k] = v
	}
	return result
}

// =============================================================================
// 파이프라인 조립 및 실행
// =============================================================================

// Pipeline은 Receiver → Processor → Exporter를 연결하는 파이프라인이다.
type Pipeline struct {
	receiver  Receiver
	processor Processor
	exporter  Exporter
	metrics   *PipelineMetrics

	receiverToProcessor chan Span
	processorToExporter chan []Span
}

func NewPipeline(receiver Receiver, processor Processor, exporter Exporter, metrics *PipelineMetrics) *Pipeline {
	return &Pipeline{
		receiver:  receiver,
		processor: processor,
		exporter:  exporter,
		metrics:   metrics,
		// 채널 버퍼: 백프레셔(backpressure) 제어
		receiverToProcessor: make(chan Span, 100),
		processorToExporter: make(chan []Span, 10),
	}
}

func (p *Pipeline) Start() {
	fmt.Printf("파이프라인 시작: %s → %s → %s\n",
		p.receiver.Name(), p.processor.Name(), p.exporter.Name())
	fmt.Println()

	// 역순으로 시작 (데이터 유실 방지)
	p.exporter.Start(p.processorToExporter)
	p.processor.Start(p.receiverToProcessor, p.processorToExporter)
	p.receiver.Start(p.receiverToProcessor)
}

func (p *Pipeline) Shutdown() {
	fmt.Println()
	fmt.Println("파이프라인 종료 시작...")

	// 순방향으로 종료 (각 단계가 남은 데이터를 플러시)
	p.receiver.Stop()
	fmt.Println("  [1/3] Receiver 종료 완료")

	// Receiver 종료 후 채널을 닫아 Processor에 EOF 신호 전달
	close(p.receiverToProcessor)
	time.Sleep(100 * time.Millisecond) // Processor가 남은 배치를 플러시할 시간

	p.processor.Stop()
	fmt.Println("  [2/3] Processor 종료 완료")

	close(p.processorToExporter)
	time.Sleep(50 * time.Millisecond)

	p.exporter.Stop()
	fmt.Println("  [3/3] Exporter 종료 완료")

	fmt.Println("파이프라인 종료 완료")
}

// =============================================================================
// 샘플 Span 생성
// =============================================================================

func generateSampleSpans() []Span {
	services := []struct {
		name       string
		operations []string
	}{
		{"frontend", []string{"HTTP GET /api/users", "HTTP POST /api/orders", "HTTP GET /api/products"}},
		{"user-service", []string{"gRPC GetUser", "gRPC ListUsers", "DB SELECT users"}},
		{"order-service", []string{"gRPC CreateOrder", "gRPC GetOrder", "DB INSERT orders"}},
		{"payment-service", []string{"gRPC ProcessPayment", "HTTP POST /gateway/charge"}},
		{"notification-service", []string{"SendEmail", "SendSMS"}},
	}

	spans := make([]Span, 0, 20)
	baseTime := time.Now()

	// 3개의 Trace를 생성 (각각 여러 Span)
	for t := 0; t < 3; t++ {
		traceID := fmt.Sprintf("%032x", rand.Int63())
		traceStart := baseTime.Add(time.Duration(t) * 500 * time.Millisecond)

		// 각 Trace에 4~7개의 Span 생성
		spanCount := 4 + rand.Intn(4)
		for s := 0; s < spanCount; s++ {
			svcIdx := s % len(services)
			svc := services[svcIdx]
			op := svc.operations[rand.Intn(len(svc.operations))]

			spans = append(spans, Span{
				TraceID:       traceID,
				SpanID:        fmt.Sprintf("%016x", rand.Int63()),
				OperationName: op,
				ServiceName:   svc.name,
				StartTime:     traceStart.Add(time.Duration(s) * 50 * time.Millisecond),
				Duration:      time.Duration(10+rand.Intn(200)) * time.Millisecond,
				Tags: map[string]string{
					"span.kind": []string{"server", "client", "producer"}[rand.Intn(3)],
					"component": svc.name,
				},
			})
		}
	}

	return spans
}

// =============================================================================
// 메인 실행
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jaeger Trace Collection Pipeline 시뮬레이션                  ║")
	fmt.Println("║  (OpenTelemetry Collector 기반 Receiver→Processor→Exporter) ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// ─── 파이프라인 아키텍처 설명 ───
	fmt.Println("=== 파이프라인 아키텍처 ===")
	fmt.Println()
	fmt.Println("  ┌──────────────┐    ┌───────────────────┐    ┌───────────────┐")
	fmt.Println("  │  HTTP        │    │  Batch            │    │  Storage      │")
	fmt.Println("  │  Receiver    │───>│  Processor        │───>│  Exporter     │")
	fmt.Println("  │              │    │                   │    │               │")
	fmt.Println("  │  JSON Span   │    │  maxBatchSize: 3  │    │  In-Memory    │")
	fmt.Println("  │  수신        │    │  flushInterval:   │    │  Map 저장     │")
	fmt.Println("  │              │    │    200ms           │    │               │")
	fmt.Println("  └──────────────┘    └───────────────────┘    └───────────────┘")
	fmt.Println()
	fmt.Println("  배치 프로세서 플러시 조건:")
	fmt.Println("  1. 배치 크기 도달 (maxBatchSize=3) → 즉시 플러시")
	fmt.Println("  2. 타이머 만료 (flushInterval=200ms) → 남은 Span 플러시")
	fmt.Println()

	// ─── 파이프라인 구성 ───
	metrics := &PipelineMetrics{}
	sampleSpans := generateSampleSpans()

	fmt.Printf("=== 생성된 샘플 Span: %d개 (3개 Trace) ===\n", len(sampleSpans))
	fmt.Println()

	receiver := NewHTTPReceiver(metrics, sampleSpans, 30*time.Millisecond)
	processor := NewBatchProcessor(3, 200*time.Millisecond, metrics)
	exporter := NewStorageExporter(metrics)

	pipeline := NewPipeline(receiver, processor, exporter, metrics)

	// ─── 파이프라인 실행 ───
	fmt.Println("=== 파이프라인 실행 시작 ===")
	fmt.Println()

	pipeline.Start()

	// Span 처리 완료 대기
	estimatedDuration := time.Duration(len(sampleSpans)) * 30 * time.Millisecond
	time.Sleep(estimatedDuration + 500*time.Millisecond)

	pipeline.Shutdown()
	fmt.Println()

	// ─── 최종 메트릭 출력 ───
	fmt.Println("=== 파이프라인 메트릭 ===")
	fmt.Println()
	metrics.Print()
	fmt.Println()

	// ─── 저장된 Trace 확인 ───
	fmt.Println("=== 저장된 Trace 요약 ===")
	fmt.Println()
	traces := exporter.GetTraces()
	for traceID, spans := range traces {
		displayID := traceID
		if len(displayID) > 16 {
			displayID = displayID[:16] + "..."
		}
		fmt.Printf("  TraceID: %s\n", displayID)
		fmt.Printf("    Span 수: %d\n", len(spans))

		// 서비스별 그룹핑
		svcMap := make(map[string][]string)
		for _, s := range spans {
			svcMap[s.ServiceName] = append(svcMap[s.ServiceName], s.OperationName)
		}
		for svc, ops := range svcMap {
			fmt.Printf("    - %s: %s\n", svc, strings.Join(ops, ", "))
		}
		fmt.Println()
	}

	// ─── 배치 처리 동작 설명 ───
	fmt.Println("=== 배치 처리 동작 분석 ===")
	fmt.Println()
	fmt.Println("  배치 프로세서는 두 가지 메커니즘으로 처리량과 지연의 균형을 맞춘다:")
	fmt.Println()
	fmt.Printf("  1. 크기 기반 플러시 (%d회):\n", metrics.BatchFlush.Load())
	fmt.Println("     - maxBatchSize(3)에 도달하면 즉시 내보내기")
	fmt.Println("     - 고부하 상황에서 빠른 처리량 확보")
	fmt.Println()
	fmt.Printf("  2. 타이머 기반 플러시 (%d회):\n", metrics.FlushCount.Load())
	fmt.Println("     - flushInterval(200ms) 경과 시 남은 Span 내보내기")
	fmt.Println("     - 저부하 상황에서도 지연 시간 제한 보장")
	fmt.Println()
	fmt.Println("  이 패턴은 OpenTelemetry Collector의 batchprocessor에서 사용되며,")
	fmt.Println("  Jaeger v2가 OTEL Collector 위에 구축됨에 따라 동일한 메커니즘을 활용한다.")
}
