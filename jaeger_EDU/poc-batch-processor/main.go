package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Jaeger/OTel Collector 배치 프로세서 시뮬레이터
// =============================================================================
//
// Jaeger v2는 OTel Collector 기반으로 동작하며, 배치 프로세서는 핵심 컴포넌트이다.
// 이 PoC는 OTel Collector의 batchprocessor 패키지가 수행하는 핵심 로직을 시뮬레이션한다:
//
// 1. 개별 스팬을 채널로 수신
// 2. 카운트 기반(sendBatchSize) 또는 시간 기반(timeout) 배치 처리
// 3. 둘 중 하나의 임계값 도달 시 플러시
// 4. 메모리 리미터 지원 (메모리 초과 시 거부)
// 5. 메트릭 추적: 전송된 배치 수, 처리된 스팬 수, 타임아웃 트리거 횟수
// 6. 가변 인입 속도로 시뮬레이션 실행
//
// 참조: opentelemetry-collector/processor/batchprocessor/
// =============================================================================

// --- 데이터 모델 ---

// Span은 단일 트레이스 스팬을 나타낸다.
type Span struct {
	TraceID     string
	SpanID      string
	ServiceName string
	Operation   string
	Duration    time.Duration
	SizeBytes   int // 스팬의 예상 메모리 크기
}

// SpanBatch는 배치로 묶인 스팬 집합이다.
type SpanBatch struct {
	Spans     []Span
	CreatedAt time.Time
	FlushedAt time.Time
	Reason    FlushReason
}

// FlushReason은 배치가 플러시된 이유를 나타낸다.
type FlushReason int

const (
	FlushReasonSize    FlushReason = iota // sendBatchSize 도달
	FlushReasonTimeout                    // timeout 도달
	FlushReasonShutdown                   // 프로세서 종료
)

func (r FlushReason) String() string {
	switch r {
	case FlushReasonSize:
		return "크기 도달"
	case FlushReasonTimeout:
		return "타임아웃"
	case FlushReasonShutdown:
		return "종료"
	default:
		return "알 수 없음"
	}
}

// --- 배치 프로세서 설정 ---

// BatchConfig는 배치 프로세서의 설정이다.
// OTel Collector의 batchprocessor.Config에 대응한다.
type BatchConfig struct {
	// SendBatchSize: 이 개수에 도달하면 배치를 전송한다.
	// OTel 기본값: 8192
	SendBatchSize int

	// SendBatchMaxSize: 배치의 최대 크기 (0이면 제한 없음)
	// SendBatchSize보다 크거나 같아야 한다.
	SendBatchMaxSize int

	// Timeout: 마지막 플러시 이후 이 시간이 지나면 배치를 전송한다.
	// OTel 기본값: 200ms
	Timeout time.Duration

	// MemoryLimitBytes: 메모리 사용량 상한 (0이면 제한 없음)
	MemoryLimitBytes int64

	// MemorySpikeLimitBytes: 메모리 스파이크 허용 한도
	MemorySpikeLimitBytes int64
}

// DefaultBatchConfig는 기본 배치 프로세서 설정을 반환한다.
func DefaultBatchConfig() BatchConfig {
	return BatchConfig{
		SendBatchSize:         100,  // 시뮬레이션용 작은 값
		SendBatchMaxSize:      0,    // 제한 없음
		Timeout:               200 * time.Millisecond,
		MemoryLimitBytes:      50000, // 50KB (시뮬레이션용)
		MemorySpikeLimitBytes: 10000, // 10KB
	}
}

// --- 배치 프로세서 메트릭 ---

// BatchMetrics는 배치 프로세서의 메트릭을 추적한다.
type BatchMetrics struct {
	BatchesSent       int64 // 전송된 배치 수
	SpansReceived     int64 // 수신된 스팬 수
	SpansProcessed    int64 // 처리된 스팬 수 (성공적으로 배치에 포함)
	SpansDropped      int64 // 드롭된 스팬 수 (메모리 제한 초과)
	TimeoutFlushes    int64 // 타임아웃으로 인한 플러시 횟수
	SizeFlushes       int64 // 크기 도달로 인한 플러시 횟수
	ShutdownFlushes   int64 // 종료 시 플러시 횟수
	CurrentMemoryUsed int64 // 현재 메모리 사용량
	PeakMemoryUsed    int64 // 최대 메모리 사용량
}

// --- 배치 프로세서 ---

// BatchProcessor는 OTel Collector의 배치 프로세서를 시뮬레이션한다.
// opentelemetry-collector/processor/batchprocessor/batch_processor.go의
// batchProcessor 구조체에 대응한다.
type BatchProcessor struct {
	config  BatchConfig
	metrics BatchMetrics

	// 내부 상태
	inputCh    chan Span       // 스팬 수신 채널
	batch      []Span          // 현재 배치 버퍼
	batchMu    sync.Mutex      // 배치 버퍼 보호
	exporterFn func(SpanBatch) // 배치 내보내기 함수
	timer      *time.Timer     // 타임아웃 타이머
	done       chan struct{}    // 종료 신호
	wg         sync.WaitGroup

	// 메모리 추적
	currentMemory int64

	// 배치 기록 (분석용)
	batchHistory []SpanBatch
	historyMu    sync.Mutex

	// 시작/종료 시간
	startTime time.Time
}

// NewBatchProcessor는 새로운 배치 프로세서를 생성한다.
func NewBatchProcessor(config BatchConfig, exporterFn func(SpanBatch)) *BatchProcessor {
	return &BatchProcessor{
		config:     config,
		inputCh:    make(chan Span, 1000), // 버퍼링된 채널
		batch:      make([]Span, 0, config.SendBatchSize),
		exporterFn: exporterFn,
		done:       make(chan struct{}),
	}
}

// Start는 배치 프로세서를 시작한다.
// OTel Collector의 batchProcessor.Start()에 대응한다.
func (bp *BatchProcessor) Start() {
	bp.startTime = time.Now()
	bp.timer = time.NewTimer(bp.config.Timeout)

	bp.wg.Add(1)
	go bp.processLoop()

	fmt.Printf("[배치 프로세서] 시작됨 (배치크기=%d, 타임아웃=%v, 메모리한도=%d바이트)\n",
		bp.config.SendBatchSize, bp.config.Timeout, bp.config.MemoryLimitBytes)
}

// processLoop는 배치 프로세서의 메인 루프이다.
// OTel Collector의 batchProcessor.processLoop()에 대응한다.
// 채널에서 스팬을 수신하고, 배치 크기 또는 타임아웃에 도달하면 플러시한다.
func (bp *BatchProcessor) processLoop() {
	defer bp.wg.Done()

	for {
		select {
		case span, ok := <-bp.inputCh:
			if !ok {
				// 채널 닫힘 — 남은 배치 플러시
				bp.flush(FlushReasonShutdown)
				return
			}

			atomic.AddInt64(&bp.metrics.SpansReceived, 1)

			// 메모리 리미터 확인
			if bp.config.MemoryLimitBytes > 0 {
				newMemory := atomic.LoadInt64(&bp.currentMemory) + int64(span.SizeBytes)
				if newMemory > bp.config.MemoryLimitBytes {
					atomic.AddInt64(&bp.metrics.SpansDropped, 1)
					continue
				}
			}

			bp.batchMu.Lock()
			bp.batch = append(bp.batch, span)
			atomic.AddInt64(&bp.currentMemory, int64(span.SizeBytes))

			// 피크 메모리 업데이트
			current := atomic.LoadInt64(&bp.currentMemory)
			peak := atomic.LoadInt64(&bp.metrics.PeakMemoryUsed)
			if current > peak {
				atomic.StoreInt64(&bp.metrics.PeakMemoryUsed, current)
			}

			atomic.StoreInt64(&bp.metrics.CurrentMemoryUsed, current)

			shouldFlush := len(bp.batch) >= bp.config.SendBatchSize
			bp.batchMu.Unlock()

			if shouldFlush {
				bp.flush(FlushReasonSize)
				// 타이머 리셋
				if !bp.timer.Stop() {
					select {
					case <-bp.timer.C:
					default:
					}
				}
				bp.timer.Reset(bp.config.Timeout)
			}

		case <-bp.timer.C:
			// 타임아웃 — 현재 배치가 비어있지 않으면 플러시
			bp.batchMu.Lock()
			hasItems := len(bp.batch) > 0
			bp.batchMu.Unlock()

			if hasItems {
				bp.flush(FlushReasonTimeout)
			}
			bp.timer.Reset(bp.config.Timeout)

		case <-bp.done:
			// 종료 요청
			bp.flush(FlushReasonShutdown)
			return
		}
	}
}

// flush는 현재 배치를 내보내기 함수에 전달한다.
func (bp *BatchProcessor) flush(reason FlushReason) {
	bp.batchMu.Lock()
	if len(bp.batch) == 0 {
		bp.batchMu.Unlock()
		return
	}

	// 현재 배치를 복사하고 새 배치로 교체
	batch := SpanBatch{
		Spans:     bp.batch,
		CreatedAt: bp.startTime,
		FlushedAt: time.Now(),
		Reason:    reason,
	}

	// SendBatchMaxSize 적용
	if bp.config.SendBatchMaxSize > 0 && len(batch.Spans) > bp.config.SendBatchMaxSize {
		batch.Spans = batch.Spans[:bp.config.SendBatchMaxSize]
		// 나머지는 다음 배치로
		remaining := bp.batch[bp.config.SendBatchMaxSize:]
		bp.batch = make([]Span, len(remaining), bp.config.SendBatchSize)
		copy(bp.batch, remaining)
	} else {
		bp.batch = make([]Span, 0, bp.config.SendBatchSize)
	}

	// 메모리 해제
	var freedMemory int64
	for _, s := range batch.Spans {
		freedMemory += int64(s.SizeBytes)
	}
	atomic.AddInt64(&bp.currentMemory, -freedMemory)

	bp.batchMu.Unlock()

	// 메트릭 업데이트
	atomic.AddInt64(&bp.metrics.BatchesSent, 1)
	atomic.AddInt64(&bp.metrics.SpansProcessed, int64(len(batch.Spans)))

	switch reason {
	case FlushReasonSize:
		atomic.AddInt64(&bp.metrics.SizeFlushes, 1)
	case FlushReasonTimeout:
		atomic.AddInt64(&bp.metrics.TimeoutFlushes, 1)
	case FlushReasonShutdown:
		atomic.AddInt64(&bp.metrics.ShutdownFlushes, 1)
	}

	// 기록 저장
	bp.historyMu.Lock()
	bp.batchHistory = append(bp.batchHistory, batch)
	bp.historyMu.Unlock()

	// 내보내기 실행
	bp.exporterFn(batch)
}

// ConsumeSpan은 스팬을 배치 프로세서에 전달한다.
// 메모리 한도 초과 시 false를 반환한다.
func (bp *BatchProcessor) ConsumeSpan(span Span) bool {
	select {
	case bp.inputCh <- span:
		return true
	case <-bp.done:
		return false
	}
}

// Shutdown은 배치 프로세서를 종료한다.
// OTel Collector의 batchProcessor.Shutdown()에 대응한다.
func (bp *BatchProcessor) Shutdown() {
	close(bp.done)
	close(bp.inputCh)
	bp.wg.Wait()
	bp.timer.Stop()
	fmt.Println("[배치 프로세서] 종료됨")
}

// PrintMetrics는 수집된 메트릭을 출력한다.
func (bp *BatchProcessor) PrintMetrics() {
	fmt.Println("\n=== 배치 프로세서 메트릭 ===")
	fmt.Println()
	fmt.Printf("  수신된 스팬:      %d\n", atomic.LoadInt64(&bp.metrics.SpansReceived))
	fmt.Printf("  처리된 스팬:      %d\n", atomic.LoadInt64(&bp.metrics.SpansProcessed))
	fmt.Printf("  드롭된 스팬:      %d\n", atomic.LoadInt64(&bp.metrics.SpansDropped))
	fmt.Println()
	fmt.Printf("  전송된 배치:      %d\n", atomic.LoadInt64(&bp.metrics.BatchesSent))
	fmt.Printf("    크기 도달:      %d\n", atomic.LoadInt64(&bp.metrics.SizeFlushes))
	fmt.Printf("    타임아웃:       %d\n", atomic.LoadInt64(&bp.metrics.TimeoutFlushes))
	fmt.Printf("    종료 시:        %d\n", atomic.LoadInt64(&bp.metrics.ShutdownFlushes))
	fmt.Println()
	fmt.Printf("  피크 메모리:      %d 바이트\n", atomic.LoadInt64(&bp.metrics.PeakMemoryUsed))
	fmt.Printf("  현재 메모리:      %d 바이트\n", atomic.LoadInt64(&bp.metrics.CurrentMemoryUsed))
}

// PrintBatchHistory는 배치 전송 기록을 출력한다.
func (bp *BatchProcessor) PrintBatchHistory() {
	bp.historyMu.Lock()
	defer bp.historyMu.Unlock()

	fmt.Println("\n=== 배치 전송 기록 ===")
	fmt.Println()
	fmt.Printf("  %-6s %-12s %-12s %-8s\n", "번호", "스팬 수", "플러시 사유", "시점")
	fmt.Println("  " + strings.Repeat("-", 44))

	for i, batch := range bp.batchHistory {
		elapsed := batch.FlushedAt.Sub(bp.startTime)
		fmt.Printf("  %-6d %-12d %-12s %8s\n",
			i+1, len(batch.Spans), batch.Reason.String(), formatDuration(elapsed))
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.0fus", float64(d)/float64(time.Microsecond))
	}
	if d < time.Second {
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// =============================================================================
// 스팬 생성기 (시뮬레이션용)
// =============================================================================

// SpanGenerator는 가변 속도로 스팬을 생성한다.
type SpanGenerator struct {
	services   []string
	operations []string
	rng        *rand.Rand
}

// NewSpanGenerator는 새로운 스팬 생성기를 만든다.
func NewSpanGenerator() *SpanGenerator {
	return &SpanGenerator{
		services:   []string{"frontend", "api-gateway", "user-service", "order-service", "database"},
		operations: []string{"GET", "POST", "PUT", "query", "insert", "update", "validate", "process"},
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Generate는 랜덤 스팬을 생성한다.
func (sg *SpanGenerator) Generate() Span {
	service := sg.services[sg.rng.Intn(len(sg.services))]
	op := sg.operations[sg.rng.Intn(len(sg.operations))]
	return Span{
		TraceID:     fmt.Sprintf("trace-%06d", sg.rng.Intn(100000)),
		SpanID:      fmt.Sprintf("span-%08d", sg.rng.Intn(100000000)),
		ServiceName: service,
		Operation:   op,
		Duration:    time.Duration(sg.rng.Intn(500)+10) * time.Millisecond,
		SizeBytes:   sg.rng.Intn(400) + 100, // 100~500 바이트
	}
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("================================================================")
	fmt.Println(" Jaeger/OTel Collector 배치 프로세서 시뮬레이터")
	fmt.Println("================================================================")

	// ── 시나리오 1: 기본 배치 처리 (크기 기반 + 타임아웃 기반) ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 1: 기본 배치 처리")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("설정: 배치크기=20, 타임아웃=300ms, 메모리 제한=없음")
	fmt.Println("동작: 총 95개 스팬을 일정 속도로 전송")
	fmt.Println()

	config1 := BatchConfig{
		SendBatchSize:    20,
		SendBatchMaxSize: 0,
		Timeout:          300 * time.Millisecond,
		MemoryLimitBytes: 0, // 메모리 제한 없음
	}

	var exportedBatches1 int64
	var exportedSpans1 int64
	exporter1 := func(batch SpanBatch) {
		atomic.AddInt64(&exportedBatches1, 1)
		atomic.AddInt64(&exportedSpans1, int64(len(batch.Spans)))
		fmt.Printf("  [내보내기] 배치 #%d: %d개 스팬 (사유: %s)\n",
			atomic.LoadInt64(&exportedBatches1), len(batch.Spans), batch.Reason)
	}

	bp1 := NewBatchProcessor(config1, exporter1)
	gen := NewSpanGenerator()

	bp1.Start()

	// 95개 스팬 전송 — 20개씩 4배치 = 80개는 크기 도달, 나머지 15개는 타임아웃으로 플러시
	for i := 0; i < 95; i++ {
		span := gen.Generate()
		bp1.ConsumeSpan(span)
		time.Sleep(5 * time.Millisecond) // 5ms 간격으로 전송
	}

	// 타임아웃 대기 (나머지 스팬이 플러시되도록)
	time.Sleep(500 * time.Millisecond)

	bp1.Shutdown()
	bp1.PrintMetrics()
	bp1.PrintBatchHistory()

	fmt.Printf("\n결과: 총 내보낸 배치=%d, 총 내보낸 스팬=%d\n",
		atomic.LoadInt64(&exportedBatches1), atomic.LoadInt64(&exportedSpans1))

	// ── 시나리오 2: 가변 인입 속도 ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 2: 가변 인입 속도")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("설정: 배치크기=30, 타임아웃=200ms, 메모리 제한=없음")
	fmt.Println("동작: 3단계로 인입 속도 변화")
	fmt.Println("  Phase 1: 저속 (20ms 간격, 30개) - 주로 타임아웃 플러시")
	fmt.Println("  Phase 2: 고속 (1ms 간격, 100개) - 주로 크기 도달 플러시")
	fmt.Println("  Phase 3: 저속 (50ms 간격, 15개) - 타임아웃 플러시")
	fmt.Println()

	config2 := BatchConfig{
		SendBatchSize:    30,
		SendBatchMaxSize: 0,
		Timeout:          200 * time.Millisecond,
		MemoryLimitBytes: 0,
	}

	exporter2 := func(batch SpanBatch) {
		fmt.Printf("  [내보내기] %d개 스팬 (사유: %s)\n",
			len(batch.Spans), batch.Reason)
	}

	bp2 := NewBatchProcessor(config2, exporter2)
	bp2.Start()

	// Phase 1: 저속
	fmt.Println("\n--- Phase 1: 저속 인입 (20ms 간격) ---")
	for i := 0; i < 30; i++ {
		bp2.ConsumeSpan(gen.Generate())
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // 나머지 플러시 대기

	// Phase 2: 고속
	fmt.Println("\n--- Phase 2: 고속 인입 (1ms 간격) ---")
	for i := 0; i < 100; i++ {
		bp2.ConsumeSpan(gen.Generate())
		time.Sleep(1 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // 나머지 플러시 대기

	// Phase 3: 저속
	fmt.Println("\n--- Phase 3: 저속 인입 (50ms 간격) ---")
	for i := 0; i < 15; i++ {
		bp2.ConsumeSpan(gen.Generate())
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	bp2.Shutdown()
	bp2.PrintMetrics()
	bp2.PrintBatchHistory()

	// ── 시나리오 3: 메모리 리미터 동작 ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 3: 메모리 리미터 동작")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("설정: 배치크기=50, 타임아웃=500ms, 메모리한도=10000바이트")
	fmt.Println("동작: 메모리 한도를 초과하면 스팬을 드롭")
	fmt.Println("  각 스팬 크기: 100~500 바이트")
	fmt.Println("  메모리 한도 10000 바이트 → 약 20~100개 스팬 수용 가능")
	fmt.Println()

	config3 := BatchConfig{
		SendBatchSize:         50,
		SendBatchMaxSize:      0,
		Timeout:               500 * time.Millisecond,
		MemoryLimitBytes:      10000, // 10KB
		MemorySpikeLimitBytes: 2000,
	}

	exporter3 := func(batch SpanBatch) {
		totalSize := 0
		for _, s := range batch.Spans {
			totalSize += s.SizeBytes
		}
		fmt.Printf("  [내보내기] %d개 스팬, %d 바이트 (사유: %s)\n",
			len(batch.Spans), totalSize, batch.Reason)
	}

	bp3 := NewBatchProcessor(config3, exporter3)
	bp3.Start()

	// 빠르게 많은 스팬 전송 — 메모리 한도 초과 시 드롭 발생
	fmt.Println("--- 200개 스팬을 빠르게 전송 (일부 드롭 예상) ---")
	for i := 0; i < 200; i++ {
		bp3.ConsumeSpan(gen.Generate())
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(600 * time.Millisecond)

	bp3.Shutdown()
	bp3.PrintMetrics()

	dropped := atomic.LoadInt64(&bp3.metrics.SpansDropped)
	processed := atomic.LoadInt64(&bp3.metrics.SpansProcessed)
	received := atomic.LoadInt64(&bp3.metrics.SpansReceived)
	fmt.Printf("\n  드롭률: %.1f%% (%d/%d)\n",
		float64(dropped)/float64(received)*100, dropped, received)
	fmt.Printf("  처리률: %.1f%% (%d/%d)\n",
		float64(processed)/float64(received)*100, processed, received)

	// ── 시나리오 4: SendBatchMaxSize 동작 ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 4: SendBatchMaxSize 제한")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("설정: 배치크기=20, 최대배치크기=25, 타임아웃=1s")
	fmt.Println("동작: 배치가 최대 크기를 초과하지 않도록 분할")
	fmt.Println()

	config4 := BatchConfig{
		SendBatchSize:    20,
		SendBatchMaxSize: 25,
		Timeout:          1 * time.Second,
		MemoryLimitBytes: 0,
	}

	exporter4 := func(batch SpanBatch) {
		fmt.Printf("  [내보내기] %d개 스팬 (사유: %s)\n",
			len(batch.Spans), batch.Reason)
	}

	bp4 := NewBatchProcessor(config4, exporter4)
	bp4.Start()

	// 50개 스팬 빠르게 전송
	for i := 0; i < 50; i++ {
		bp4.ConsumeSpan(gen.Generate())
		time.Sleep(1 * time.Millisecond)
	}
	time.Sleep(1200 * time.Millisecond)

	bp4.Shutdown()
	bp4.PrintMetrics()
	bp4.PrintBatchHistory()

	// ── 전체 요약 ──
	fmt.Println("\n\n================================================================")
	fmt.Println(" 시뮬레이션 완료")
	fmt.Println("================================================================")
	fmt.Println()
	fmt.Println("=== OTel Collector 배치 프로세서 핵심 설계 포인트 ===")
	fmt.Println()
	fmt.Println("1. 이중 트리거: 배치 크기(sendBatchSize)와 타임아웃(timeout) 중")
	fmt.Println("   먼저 도달하는 조건으로 플러시를 트리거한다.")
	fmt.Println("   - 고트래픽: 크기 도달로 빈번한 플러시")
	fmt.Println("   - 저트래픽: 타임아웃으로 지연 없이 전송")
	fmt.Println()
	fmt.Println("2. 메모리 리미터: memorylimiter 프로세서와 연동하여")
	fmt.Println("   메모리 사용량이 한도를 초과하면 새 스팬을 거부(드롭)한다.")
	fmt.Println("   이는 OOM 방지를 위한 안전 장치이다.")
	fmt.Println()
	fmt.Println("3. SendBatchMaxSize: 배치 크기에 상한을 두어 단일 배치가")
	fmt.Println("   과도하게 커지는 것을 방지한다. 초과 스팬은 다음 배치로 이월.")
	fmt.Println()
	fmt.Println("4. 라이프사이클: Start()로 프로세스 루프 시작, Shutdown()으로")
	fmt.Println("   남은 배치를 플러시한 후 정상 종료한다. 이는 데이터 손실 방지.")
	fmt.Println()
	fmt.Println("5. 메트릭: 배치 전송 횟수, 플러시 사유별 횟수, 드롭 수 등을")
	fmt.Println("   추적하여 프로세서 동작을 모니터링할 수 있게 한다.")
}
