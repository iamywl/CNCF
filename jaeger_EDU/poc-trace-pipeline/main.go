package main

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Jaeger 트레이스 파이프라인 시뮬레이터
// =============================================================================
//
// Jaeger v2는 OTel Collector의 Receiver → Processor → Exporter 파이프라인
// 패턴을 그대로 채택한다. 이 PoC는 해당 파이프라인의 전체 구조를 시뮬레이션한다:
//
// 1. 컴포넌트 인터페이스 정의 (OTel Collector 패턴)
//    - Receiver: Start(), Shutdown()
//    - Processor: ProcessTraces(traces)
//    - Exporter: ExportTraces(traces)
//
// 2. 파이프라인 빌더: 컴포넌트를 체이닝하여 파이프라인 구성
//
// 3. 다중 리시버 (OTLP 시뮬레이터, Jaeger 시뮬레이터)
// 4. 다중 프로세서 (배치, 필터, 테일 샘플링)
// 5. 다중 익스포터 (메모리 저장소, 콘솔)
// 6. 팬인(Fan-in) → 체이닝 → 팬아웃(Fan-out)
// 7. 라이프사이클 관리: Start all → process → Shutdown all
//
// 참조:
// - opentelemetry-collector/service/pipelines/
// - jaeger/cmd/jaeger/internal/
// =============================================================================

// --- 공통 데이터 모델 ---

// Span은 단일 스팬을 나타낸다.
type Span struct {
	TraceID     string
	SpanID      string
	ParentID    string
	ServiceName string
	Operation   string
	StartTime   time.Time
	Duration    time.Duration
	Tags        map[string]string
	HasError    bool
}

// Traces는 스팬의 집합으로, 파이프라인을 통해 전달되는 데이터 단위이다.
// OTel Collector의 pdata.Traces에 대응한다.
type Traces struct {
	Spans []Span
}

// --- 컴포넌트 인터페이스 ---
// OTel Collector의 component 패키지의 인터페이스를 반영한다.

// Component는 모든 파이프라인 컴포넌트의 기본 인터페이스이다.
type Component interface {
	// Start는 컴포넌트를 시작한다.
	Start(ctx context.Context) error
	// Shutdown은 컴포넌트를 정상 종료한다.
	Shutdown(ctx context.Context) error
	// Name은 컴포넌트 이름을 반환한다.
	Name() string
}

// Receiver는 외부로부터 데이터를 수신하는 컴포넌트이다.
// OTel Collector의 receiver.Traces에 대응한다.
type Receiver interface {
	Component
}

// Processor는 데이터를 변환/필터링하는 컴포넌트이다.
// OTel Collector의 processor.Traces에 대응한다.
type Processor interface {
	Component
	ProcessTraces(ctx context.Context, td Traces) (Traces, error)
}

// Exporter는 데이터를 외부로 내보내는 컴포넌트이다.
// OTel Collector의 exporter.Traces에 대응한다.
type Exporter interface {
	Component
	ExportTraces(ctx context.Context, td Traces) error
}

// Consumer는 Traces를 소비하는 인터페이스이다.
// Receiver가 데이터를 전달할 대상이다.
type Consumer interface {
	ConsumeTraces(ctx context.Context, td Traces) error
}

// --- 리시버 구현 ---

// OTLPReceiver는 OTLP 프로토콜을 시뮬레이션하는 리시버이다.
// Jaeger v2에서 기본 리시버로 사용된다.
type OTLPReceiver struct {
	name     string
	consumer Consumer
	done     chan struct{}
	wg       sync.WaitGroup
	rng      *rand.Rand
	rate     time.Duration // 스팬 생성 간격
	count    int           // 생성할 총 스팬 수
	sent     int64
}

func NewOTLPReceiver(name string, consumer Consumer, rate time.Duration, count int) *OTLPReceiver {
	return &OTLPReceiver{
		name:     name,
		consumer: consumer,
		done:     make(chan struct{}),
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
		rate:     rate,
		count:    count,
	}
}

func (r *OTLPReceiver) Name() string { return r.name }

func (r *OTLPReceiver) Start(ctx context.Context) error {
	fmt.Printf("  [리시버] %s 시작\n", r.name)
	r.wg.Add(1)
	go r.generateSpans(ctx)
	return nil
}

func (r *OTLPReceiver) generateSpans(ctx context.Context) {
	defer r.wg.Done()

	services := []string{"web-frontend", "api-server", "auth-service", "db-proxy"}
	operations := []string{"HTTP GET", "HTTP POST", "gRPC call", "SQL query"}

	for i := 0; i < r.count; i++ {
		select {
		case <-r.done:
			return
		case <-ctx.Done():
			return
		default:
		}

		traceID := fmt.Sprintf("otlp-trace-%04d", i/3)
		span := Span{
			TraceID:     traceID,
			SpanID:      fmt.Sprintf("otlp-span-%06d", i),
			ServiceName: services[r.rng.Intn(len(services))],
			Operation:   operations[r.rng.Intn(len(operations))],
			StartTime:   time.Now(),
			Duration:    time.Duration(r.rng.Intn(200)+10) * time.Millisecond,
			Tags:        map[string]string{"receiver": "otlp", "format": "protobuf"},
			HasError:    r.rng.Float64() < 0.1, // 10% 에러율
		}

		traces := Traces{Spans: []Span{span}}
		if err := r.consumer.ConsumeTraces(ctx, traces); err != nil {
			// 에러 시 스킵
			continue
		}
		atomic.AddInt64(&r.sent, 1)

		time.Sleep(r.rate)
	}
}

func (r *OTLPReceiver) Shutdown(ctx context.Context) error {
	close(r.done)
	r.wg.Wait()
	fmt.Printf("  [리시버] %s 종료 (전송: %d 스팬)\n", r.name, atomic.LoadInt64(&r.sent))
	return nil
}

// JaegerReceiver는 Jaeger 네이티브 프로토콜을 시뮬레이션하는 리시버이다.
// Jaeger v2에서 하위 호환성을 위해 지원한다.
type JaegerReceiver struct {
	name     string
	consumer Consumer
	done     chan struct{}
	wg       sync.WaitGroup
	rng      *rand.Rand
	rate     time.Duration
	count    int
	sent     int64
}

func NewJaegerReceiver(name string, consumer Consumer, rate time.Duration, count int) *JaegerReceiver {
	return &JaegerReceiver{
		name:     name,
		consumer: consumer,
		done:     make(chan struct{}),
		rng:      rand.New(rand.NewSource(time.Now().UnixNano() + 1000)),
		rate:     rate,
		count:    count,
	}
}

func (r *JaegerReceiver) Name() string { return r.name }

func (r *JaegerReceiver) Start(ctx context.Context) error {
	fmt.Printf("  [리시버] %s 시작\n", r.name)
	r.wg.Add(1)
	go r.generateSpans(ctx)
	return nil
}

func (r *JaegerReceiver) generateSpans(ctx context.Context) {
	defer r.wg.Done()

	services := []string{"order-service", "payment-service", "inventory-service", "notification-service"}
	operations := []string{"processOrder", "chargePayment", "checkStock", "sendEmail"}

	for i := 0; i < r.count; i++ {
		select {
		case <-r.done:
			return
		case <-ctx.Done():
			return
		default:
		}

		traceID := fmt.Sprintf("jaeger-trace-%04d", i/3)
		span := Span{
			TraceID:     traceID,
			SpanID:      fmt.Sprintf("jaeger-span-%06d", i),
			ServiceName: services[r.rng.Intn(len(services))],
			Operation:   operations[r.rng.Intn(len(operations))],
			StartTime:   time.Now(),
			Duration:    time.Duration(r.rng.Intn(300)+20) * time.Millisecond,
			Tags:        map[string]string{"receiver": "jaeger", "format": "thrift"},
			HasError:    r.rng.Float64() < 0.15, // 15% 에러율
		}

		traces := Traces{Spans: []Span{span}}
		if err := r.consumer.ConsumeTraces(ctx, traces); err != nil {
			continue
		}
		atomic.AddInt64(&r.sent, 1)

		time.Sleep(r.rate)
	}
}

func (r *JaegerReceiver) Shutdown(ctx context.Context) error {
	close(r.done)
	r.wg.Wait()
	fmt.Printf("  [리시버] %s 종료 (전송: %d 스팬)\n", r.name, atomic.LoadInt64(&r.sent))
	return nil
}

// --- 프로세서 구현 ---

// BatchProcessor는 스팬을 배치로 묶어 처리하는 프로세서이다.
type BatchProcessor struct {
	name      string
	batchSize int
	buffer    []Span
	mu        sync.Mutex
	processed int64
	batches   int64
}

func NewBatchProcessor(name string, batchSize int) *BatchProcessor {
	return &BatchProcessor{
		name:      name,
		batchSize: batchSize,
		buffer:    make([]Span, 0, batchSize),
	}
}

func (p *BatchProcessor) Name() string                         { return p.name }
func (p *BatchProcessor) Start(ctx context.Context) error      { fmt.Printf("  [프로세서] %s 시작\n", p.name); return nil }
func (p *BatchProcessor) Shutdown(ctx context.Context) error {
	fmt.Printf("  [프로세서] %s 종료 (처리: %d 스팬, %d 배치)\n",
		p.name, atomic.LoadInt64(&p.processed), atomic.LoadInt64(&p.batches))
	return nil
}

func (p *BatchProcessor) ProcessTraces(ctx context.Context, td Traces) (Traces, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.buffer = append(p.buffer, td.Spans...)
	atomic.AddInt64(&p.processed, int64(len(td.Spans)))

	// 배치 크기에 도달하면 전체 버퍼를 반환
	if len(p.buffer) >= p.batchSize {
		result := Traces{Spans: make([]Span, len(p.buffer))}
		copy(result.Spans, p.buffer)
		p.buffer = p.buffer[:0]
		atomic.AddInt64(&p.batches, 1)
		return result, nil
	}

	// 아직 배치 미완성 — 빈 Traces 반환 (다음 단계로 전달하지 않음)
	return Traces{}, nil
}

// FlushRemaining은 버퍼에 남은 스팬을 반환한다.
func (p *BatchProcessor) FlushRemaining() Traces {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buffer) == 0 {
		return Traces{}
	}
	result := Traces{Spans: make([]Span, len(p.buffer))}
	copy(result.Spans, p.buffer)
	p.buffer = p.buffer[:0]
	atomic.AddInt64(&p.batches, 1)
	return result
}

// FilterProcessor는 특정 서비스의 스팬만 통과시키는 프로세서이다.
type FilterProcessor struct {
	name            string
	allowedServices map[string]bool
	passed          int64
	filtered        int64
}

func NewFilterProcessor(name string, services []string) *FilterProcessor {
	allowed := make(map[string]bool)
	for _, s := range services {
		allowed[s] = true
	}
	return &FilterProcessor{
		name:            name,
		allowedServices: allowed,
	}
}

func (p *FilterProcessor) Name() string                         { return p.name }
func (p *FilterProcessor) Start(ctx context.Context) error      { fmt.Printf("  [프로세서] %s 시작\n", p.name); return nil }
func (p *FilterProcessor) Shutdown(ctx context.Context) error {
	fmt.Printf("  [프로세서] %s 종료 (통과: %d, 필터링: %d)\n",
		p.name, atomic.LoadInt64(&p.passed), atomic.LoadInt64(&p.filtered))
	return nil
}

func (p *FilterProcessor) ProcessTraces(ctx context.Context, td Traces) (Traces, error) {
	if len(td.Spans) == 0 {
		return td, nil
	}

	var filtered []Span
	for _, span := range td.Spans {
		if p.allowedServices[span.ServiceName] {
			filtered = append(filtered, span)
			atomic.AddInt64(&p.passed, 1)
		} else {
			atomic.AddInt64(&p.filtered, 1)
		}
	}

	return Traces{Spans: filtered}, nil
}

// TailSamplingProcessor는 테일 기반 샘플링을 수행하는 프로세서이다.
// 전체 트레이스를 본 후 샘플링 결정을 내린다.
type TailSamplingProcessor struct {
	name        string
	sampleRate  float64 // 0.0~1.0
	errorAlways bool    // 에러 스팬은 항상 유지
	rng         *rand.Rand
	sampled     int64
	dropped     int64
}

func NewTailSamplingProcessor(name string, sampleRate float64, errorAlways bool) *TailSamplingProcessor {
	return &TailSamplingProcessor{
		name:        name,
		sampleRate:  sampleRate,
		errorAlways: errorAlways,
		rng:         rand.New(rand.NewSource(time.Now().UnixNano() + 9999)),
	}
}

func (p *TailSamplingProcessor) Name() string                         { return p.name }
func (p *TailSamplingProcessor) Start(ctx context.Context) error      { fmt.Printf("  [프로세서] %s 시작 (샘플률: %.0f%%)\n", p.name, p.sampleRate*100); return nil }
func (p *TailSamplingProcessor) Shutdown(ctx context.Context) error {
	fmt.Printf("  [프로세서] %s 종료 (샘플링: %d, 드롭: %d)\n",
		p.name, atomic.LoadInt64(&p.sampled), atomic.LoadInt64(&p.dropped))
	return nil
}

func (p *TailSamplingProcessor) ProcessTraces(ctx context.Context, td Traces) (Traces, error) {
	if len(td.Spans) == 0 {
		return td, nil
	}

	var sampled []Span
	for _, span := range td.Spans {
		keep := false

		// 에러 스팬은 항상 유지
		if p.errorAlways && span.HasError {
			keep = true
		} else {
			// 확률적 샘플링
			keep = p.rng.Float64() < p.sampleRate
		}

		if keep {
			sampled = append(sampled, span)
			atomic.AddInt64(&p.sampled, 1)
		} else {
			atomic.AddInt64(&p.dropped, 1)
		}
	}

	return Traces{Spans: sampled}, nil
}

// --- 익스포터 구현 ---

// MemoryExporter는 메모리에 스팬을 저장하는 익스포터이다.
// Jaeger의 메모리 저장소에 대응한다.
type MemoryExporter struct {
	name      string
	spans     []Span
	mu        sync.Mutex
	exported  int64
	traces    map[string]int // traceID → 스팬 수
}

func NewMemoryExporter(name string) *MemoryExporter {
	return &MemoryExporter{
		name:   name,
		traces: make(map[string]int),
	}
}

func (e *MemoryExporter) Name() string                         { return e.name }
func (e *MemoryExporter) Start(ctx context.Context) error      { fmt.Printf("  [익스포터] %s 시작\n", e.name); return nil }
func (e *MemoryExporter) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	fmt.Printf("  [익스포터] %s 종료 (저장: %d 스팬, %d 트레이스)\n",
		e.name, atomic.LoadInt64(&e.exported), len(e.traces))
	return nil
}

func (e *MemoryExporter) ExportTraces(ctx context.Context, td Traces) error {
	if len(td.Spans) == 0 {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.spans = append(e.spans, td.Spans...)
	atomic.AddInt64(&e.exported, int64(len(td.Spans)))

	for _, span := range td.Spans {
		e.traces[span.TraceID]++
	}

	return nil
}

func (e *MemoryExporter) PrintStats() {
	e.mu.Lock()
	defer e.mu.Unlock()

	fmt.Printf("\n  [%s] 저장 통계:\n", e.name)
	fmt.Printf("    총 스팬: %d\n", len(e.spans))
	fmt.Printf("    총 트레이스: %d\n", len(e.traces))

	// 서비스별 스팬 수
	serviceCount := make(map[string]int)
	for _, span := range e.spans {
		serviceCount[span.ServiceName]++
	}
	fmt.Printf("    서비스별 스팬 수:\n")
	for svc, count := range serviceCount {
		fmt.Printf("      %-25s %d\n", svc, count)
	}
}

// ConsoleExporter는 스팬 정보를 콘솔에 출력하는 익스포터이다.
type ConsoleExporter struct {
	name     string
	verbose  bool
	exported int64
}

func NewConsoleExporter(name string, verbose bool) *ConsoleExporter {
	return &ConsoleExporter{name: name, verbose: verbose}
}

func (e *ConsoleExporter) Name() string                         { return e.name }
func (e *ConsoleExporter) Start(ctx context.Context) error      { fmt.Printf("  [익스포터] %s 시작\n", e.name); return nil }
func (e *ConsoleExporter) Shutdown(ctx context.Context) error {
	fmt.Printf("  [익스포터] %s 종료 (출력: %d 스팬)\n", e.name, atomic.LoadInt64(&e.exported))
	return nil
}

func (e *ConsoleExporter) ExportTraces(ctx context.Context, td Traces) error {
	if len(td.Spans) == 0 {
		return nil
	}

	atomic.AddInt64(&e.exported, int64(len(td.Spans)))

	if e.verbose {
		for _, span := range td.Spans {
			errMark := ""
			if span.HasError {
				errMark = " [ERROR]"
			}
			fmt.Printf("    [콘솔] %s | %s | %s | %v%s\n",
				span.TraceID, span.ServiceName, span.Operation, span.Duration, errMark)
		}
	} else {
		fmt.Printf("    [콘솔] %d개 스팬 내보냄\n", len(td.Spans))
	}

	return nil
}

// --- 파이프라인 ---

// Pipeline은 Receiver → Processor → Exporter 파이프라인이다.
// OTel Collector의 pipelines.Pipeline에 대응한다.
type Pipeline struct {
	name       string
	receivers  []Receiver
	processors []Processor
	exporters  []Exporter
	inputCh    chan Traces
	done       chan struct{}
	wg         sync.WaitGroup
}

// PipelineConfig는 파이프라인 설정이다.
type PipelineConfig struct {
	Name       string
	BufferSize int
}

// NewPipeline은 새로운 파이프라인을 생성한다.
func NewPipeline(config PipelineConfig) *Pipeline {
	bufSize := config.BufferSize
	if bufSize == 0 {
		bufSize = 100
	}
	return &Pipeline{
		name:    config.Name,
		inputCh: make(chan Traces, bufSize),
		done:    make(chan struct{}),
	}
}

// AddReceiver는 리시버를 추가한다.
func (p *Pipeline) AddReceiver(r Receiver) {
	p.receivers = append(p.receivers, r)
}

// AddProcessor는 프로세서를 추가한다 (체인 순서대로).
func (p *Pipeline) AddProcessor(proc Processor) {
	p.processors = append(p.processors, proc)
}

// AddExporter는 익스포터를 추가한다.
func (p *Pipeline) AddExporter(exp Exporter) {
	p.exporters = append(p.exporters, exp)
}

// ConsumeTraces는 Consumer 인터페이스를 구현한다.
// 리시버에서 데이터를 받아 파이프라인에 전달한다.
func (p *Pipeline) ConsumeTraces(ctx context.Context, td Traces) error {
	select {
	case p.inputCh <- td:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start는 파이프라인의 모든 컴포넌트를 시작한다.
func (p *Pipeline) Start(ctx context.Context) error {
	fmt.Printf("\n[파이프라인] '%s' 시작\n", p.name)
	fmt.Println(strings.Repeat("-", 50))

	// 1. 익스포터 먼저 시작 (데이터 수신 준비)
	fmt.Println("  >>> 익스포터 시작")
	for _, exp := range p.exporters {
		if err := exp.Start(ctx); err != nil {
			return fmt.Errorf("익스포터 %s 시작 실패: %w", exp.Name(), err)
		}
	}

	// 2. 프로세서 시작
	fmt.Println("  >>> 프로세서 시작")
	for _, proc := range p.processors {
		if err := proc.Start(ctx); err != nil {
			return fmt.Errorf("프로세서 %s 시작 실패: %w", proc.Name(), err)
		}
	}

	// 3. 파이프라인 처리 루프 시작
	p.wg.Add(1)
	go p.processLoop(ctx)

	// 4. 리시버 마지막 시작 (데이터 생성 시작)
	fmt.Println("  >>> 리시버 시작")
	for _, recv := range p.receivers {
		if err := recv.Start(ctx); err != nil {
			return fmt.Errorf("리시버 %s 시작 실패: %w", recv.Name(), err)
		}
	}

	fmt.Println(strings.Repeat("-", 50))
	return nil
}

// processLoop는 파이프라인의 메인 처리 루프이다.
// 리시버로부터 팬인(fan-in)된 데이터를 프로세서 체인에 통과시킨 후
// 모든 익스포터로 팬아웃(fan-out)한다.
func (p *Pipeline) processLoop(ctx context.Context) {
	defer p.wg.Done()

	for {
		select {
		case td, ok := <-p.inputCh:
			if !ok {
				return
			}

			// 프로세서 체인 통과
			current := td
			var err error
			skip := false

			for _, proc := range p.processors {
				current, err = proc.ProcessTraces(ctx, current)
				if err != nil {
					fmt.Printf("  [에러] 프로세서 %s: %v\n", proc.Name(), err)
					skip = true
					break
				}
				// 빈 결과면 (배치 프로세서에서 아직 버퍼링 중) 스킵
				if len(current.Spans) == 0 {
					skip = true
					break
				}
			}

			if skip {
				continue
			}

			// 모든 익스포터로 팬아웃
			for _, exp := range p.exporters {
				if err := exp.ExportTraces(ctx, current); err != nil {
					fmt.Printf("  [에러] 익스포터 %s: %v\n", exp.Name(), err)
				}
			}

		case <-p.done:
			// 남은 데이터 드레인
			for {
				select {
				case td, ok := <-p.inputCh:
					if !ok {
						return
					}
					current := td
					for _, proc := range p.processors {
						current, _ = proc.ProcessTraces(ctx, current)
						if len(current.Spans) == 0 {
							break
						}
					}
					if len(current.Spans) > 0 {
						for _, exp := range p.exporters {
							exp.ExportTraces(ctx, current)
						}
					}
				default:
					// 배치 프로세서에 남은 데이터 플러시
					for _, proc := range p.processors {
						if bp, ok := proc.(*BatchProcessor); ok {
							remaining := bp.FlushRemaining()
							if len(remaining.Spans) > 0 {
								for _, exp := range p.exporters {
									exp.ExportTraces(ctx, remaining)
								}
							}
						}
					}
					return
				}
			}
		}
	}
}

// Shutdown은 파이프라인의 모든 컴포넌트를 역순으로 종료한다.
// OTel Collector의 종료 순서: Receiver → Processor → Exporter
func (p *Pipeline) Shutdown(ctx context.Context) error {
	fmt.Printf("\n[파이프라인] '%s' 종료 시작\n", p.name)
	fmt.Println(strings.Repeat("-", 50))

	// 1. 리시버 먼저 종료 (새 데이터 수신 중단)
	fmt.Println("  >>> 리시버 종료")
	for _, recv := range p.receivers {
		if err := recv.Shutdown(ctx); err != nil {
			fmt.Printf("  [경고] 리시버 %s 종료 에러: %v\n", recv.Name(), err)
		}
	}

	// 잠시 대기 (남은 데이터 처리)
	time.Sleep(100 * time.Millisecond)

	// 2. 처리 루프 종료
	close(p.done)
	p.wg.Wait()
	close(p.inputCh)

	// 3. 프로세서 종료
	fmt.Println("  >>> 프로세서 종료")
	for _, proc := range p.processors {
		if err := proc.Shutdown(ctx); err != nil {
			fmt.Printf("  [경고] 프로세서 %s 종료 에러: %v\n", proc.Name(), err)
		}
	}

	// 4. 익스포터 마지막 종료
	fmt.Println("  >>> 익스포터 종료")
	for _, exp := range p.exporters {
		if err := exp.Shutdown(ctx); err != nil {
			fmt.Printf("  [경고] 익스포터 %s 종료 에러: %v\n", exp.Name(), err)
		}
	}

	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("[파이프라인] '%s' 종료 완료\n", p.name)
	return nil
}

// --- 파이프라인 빌더 ---

// PipelineBuilder는 파이프라인을 선언적으로 구성하는 빌더이다.
type PipelineBuilder struct {
	pipeline *Pipeline
}

func NewPipelineBuilder(name string) *PipelineBuilder {
	return &PipelineBuilder{
		pipeline: NewPipeline(PipelineConfig{Name: name, BufferSize: 200}),
	}
}

func (b *PipelineBuilder) WithReceiver(factory func(consumer Consumer) Receiver) *PipelineBuilder {
	recv := factory(b.pipeline)
	b.pipeline.AddReceiver(recv)
	return b
}

func (b *PipelineBuilder) WithProcessor(proc Processor) *PipelineBuilder {
	b.pipeline.AddProcessor(proc)
	return b
}

func (b *PipelineBuilder) WithExporter(exp Exporter) *PipelineBuilder {
	b.pipeline.AddExporter(exp)
	return b
}

func (b *PipelineBuilder) Build() *Pipeline {
	return b.pipeline
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("================================================================")
	fmt.Println(" Jaeger 트레이스 파이프라인 시뮬레이터")
	fmt.Println(" (OTel Collector Receiver -> Processor -> Exporter)")
	fmt.Println("================================================================")

	ctx := context.Background()

	// ── 시나리오 1: 단순 파이프라인 ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 1: 단순 파이프라인")
	fmt.Println("# OTLP Receiver -> Console Exporter")
	fmt.Println("########################################################")

	memExp1 := NewMemoryExporter("memory-store-1")
	consoleExp1 := NewConsoleExporter("console-1", true)

	pipeline1 := NewPipelineBuilder("simple-pipeline").
		WithReceiver(func(consumer Consumer) Receiver {
			return NewOTLPReceiver("otlp-receiver", consumer, 30*time.Millisecond, 10)
		}).
		WithExporter(memExp1).
		WithExporter(consoleExp1).
		Build()

	if err := pipeline1.Start(ctx); err != nil {
		fmt.Printf("파이프라인 시작 실패: %v\n", err)
		return
	}

	// 리시버가 모든 스팬을 전송할 때까지 대기
	time.Sleep(500 * time.Millisecond)

	pipeline1.Shutdown(ctx)
	memExp1.PrintStats()

	// ── 시나리오 2: 다중 리시버 + 프로세서 체인 ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 2: 다중 리시버 + 프로세서 체인")
	fmt.Println("# [OTLP Receiver] ─┐")
	fmt.Println("#                   ├→ Batch Processor → Tail Sampling → Memory Store")
	fmt.Println("# [Jaeger Receiver]─┘")
	fmt.Println("########################################################")

	memExp2 := NewMemoryExporter("memory-store-2")

	pipeline2 := NewPipelineBuilder("multi-receiver-pipeline").
		WithReceiver(func(consumer Consumer) Receiver {
			return NewOTLPReceiver("otlp-receiver", consumer, 15*time.Millisecond, 30)
		}).
		WithReceiver(func(consumer Consumer) Receiver {
			return NewJaegerReceiver("jaeger-receiver", consumer, 20*time.Millisecond, 20)
		}).
		WithProcessor(NewBatchProcessor("batch-processor", 10)).
		WithProcessor(NewTailSamplingProcessor("tail-sampler", 0.5, true)). // 50% 샘플링 + 에러 항상 유지
		WithExporter(memExp2).
		Build()

	if err := pipeline2.Start(ctx); err != nil {
		fmt.Printf("파이프라인 시작 실패: %v\n", err)
		return
	}

	time.Sleep(1 * time.Second)

	pipeline2.Shutdown(ctx)
	memExp2.PrintStats()

	// ── 시나리오 3: 필터 + 다중 익스포터 (팬아웃) ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 3: 필터 프로세서 + 다중 익스포터 (팬아웃)")
	fmt.Println("# OTLP Receiver → Filter(특정 서비스만) → [Memory Store]")
	fmt.Println("#                                       → [Console]")
	fmt.Println("########################################################")

	memExp3 := NewMemoryExporter("memory-store-3")
	consoleExp3 := NewConsoleExporter("console-3", false)

	pipeline3 := NewPipelineBuilder("filter-fanout-pipeline").
		WithReceiver(func(consumer Consumer) Receiver {
			return NewOTLPReceiver("otlp-receiver", consumer, 10*time.Millisecond, 40)
		}).
		WithProcessor(NewFilterProcessor("service-filter",
			[]string{"api-server", "auth-service"})). // 이 서비스만 통과
		WithExporter(memExp3).
		WithExporter(consoleExp3).
		Build()

	if err := pipeline3.Start(ctx); err != nil {
		fmt.Printf("파이프라인 시작 실패: %v\n", err)
		return
	}

	time.Sleep(700 * time.Millisecond)

	pipeline3.Shutdown(ctx)
	memExp3.PrintStats()

	// ── 시나리오 4: 전체 통합 파이프라인 ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 4: 전체 통합 파이프라인")
	fmt.Println("# [OTLP] ──┐                                    ┌→ [Memory Store]")
	fmt.Println("#           ├→ Batch → Filter → Tail Sampling ──┤")
	fmt.Println("# [Jaeger]─┘                                    └→ [Console]")
	fmt.Println("########################################################")

	memExp4 := NewMemoryExporter("memory-store-4")
	consoleExp4 := NewConsoleExporter("console-4", false)

	pipeline4 := NewPipelineBuilder("full-pipeline").
		WithReceiver(func(consumer Consumer) Receiver {
			return NewOTLPReceiver("otlp-receiver-1", consumer, 10*time.Millisecond, 50)
		}).
		WithReceiver(func(consumer Consumer) Receiver {
			return NewJaegerReceiver("jaeger-receiver-1", consumer, 12*time.Millisecond, 40)
		}).
		WithProcessor(NewBatchProcessor("batch-proc", 15)).
		WithProcessor(NewFilterProcessor("svc-filter",
			[]string{"api-server", "auth-service", "order-service", "payment-service"})).
		WithProcessor(NewTailSamplingProcessor("tail-sampler", 0.7, true)).
		WithExporter(memExp4).
		WithExporter(consoleExp4).
		Build()

	if err := pipeline4.Start(ctx); err != nil {
		fmt.Printf("파이프라인 시작 실패: %v\n", err)
		return
	}

	time.Sleep(1200 * time.Millisecond)

	pipeline4.Shutdown(ctx)
	memExp4.PrintStats()

	// ── 설계 포인트 요약 ──
	fmt.Println("\n\n================================================================")
	fmt.Println(" 시뮬레이션 완료")
	fmt.Println("================================================================")
	fmt.Println()
	fmt.Println("=== Jaeger/OTel Collector 트레이스 파이프라인 핵심 설계 포인트 ===")
	fmt.Println()
	fmt.Println("1. 컴포넌트 인터페이스: 모든 컴포넌트는 Start/Shutdown 라이프사이클과")
	fmt.Println("   데이터 처리 메서드(ProcessTraces/ExportTraces)를 구현한다.")
	fmt.Println("   이 통일된 인터페이스가 플러그인 아키텍처의 기반이다.")
	fmt.Println()
	fmt.Println("2. 파이프라인 토폴로지:")
	fmt.Println("   - Fan-in: 다중 리시버가 하나의 파이프라인으로 데이터를 보냄")
	fmt.Println("   - Chaining: 프로세서가 순서대로 체이닝되어 데이터를 변환")
	fmt.Println("   - Fan-out: 처리된 데이터가 모든 익스포터로 동시에 전달")
	fmt.Println()
	fmt.Println("3. 종료 순서 (중요!):")
	fmt.Println("   시작: Exporter → Processor → Receiver (받을 준비 먼저)")
	fmt.Println("   종료: Receiver → Processor → Exporter (보내기 먼저 중단)")
	fmt.Println("   이 순서가 데이터 손실을 방지한다.")
	fmt.Println()
	fmt.Println("4. Jaeger v2 전환: Jaeger v2는 자체 파이프라인을 OTel Collector")
	fmt.Println("   파이프라인으로 완전히 대체하여, 동일한 확장성과 유연성을 얻었다.")
}
