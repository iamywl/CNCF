package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Jaeger In-Memory Store 시뮬레이션
// =============================================================================
// Jaeger의 인메모리 스토리지(internal/storage/v2/memory)를 재현한다.
// 실제 소스 분석을 기반으로 다음 핵심 기능을 구현한다:
//
// 1. 링 버퍼 기반 Trace 저장 (max_traces 제한, 가장 오래된 항목 퇴거)
//    - 실제 소스: internal/storage/v2/memory/tenant.go
//    - traces[]는 고정 크기 배열이며, mostRecent 인덱스가 순환
//    - 새 Trace가 들어오면 (mostRecent+1) % MaxTraces 위치에 저장
//    - 해당 위치의 기존 Trace ID는 ids 맵에서 삭제
//
// 2. 서비스/오퍼레이션 인덱싱
//    - services: map[string]struct{} — 전체 서비스 목록
//    - operations: map[serviceName]map[Operation]struct{} — 서비스별 오퍼레이션
//
// 3. FindTraces 쿼리 (서비스 이름, 시간 범위 필터)
//    - mostRecent 위치에서 역순으로 탐색 (최신 Trace부터)
//    - validTrace() → validResource() → validSpan() 조건 검사
//
// 4. Thread-safe: sync.RWMutex (읽기는 공유, 쓰기는 배타적)
// =============================================================================

// =============================================================================
// 데이터 모델
// =============================================================================

// Span은 분산 추적의 기본 단위이다.
type Span struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	OperationName string
	ServiceName   string
	StartTime     time.Time
	Duration      time.Duration
	Tags          map[string]string
	SpanKind      string // server, client, producer, consumer, internal
}

// Operation은 서비스의 오퍼레이션 정보이다.
// 실제 Jaeger: internal/storage/v2/api/tracestore/reader.go의 Operation
type Operation struct {
	Name     string
	SpanKind string
}

// TraceQueryParams는 Trace 검색 조건이다.
// 실제 Jaeger: internal/storage/v2/api/tracestore/reader.go의 TraceQueryParams
type TraceQueryParams struct {
	ServiceName   string
	OperationName string
	StartTimeMin  time.Time
	StartTimeMax  time.Time
	DurationMin   time.Duration
	DurationMax   time.Duration
	SearchDepth   int // 탐색할 최대 Trace 수
}

// =============================================================================
// 내부 저장 구조
// =============================================================================

// traceRecord는 하나의 Trace를 저장하는 내부 레코드이다.
// 실제 Jaeger: internal/storage/v2/memory/tenant.go의 traceAndId
type traceRecord struct {
	traceID   string
	spans     []Span
	startTime time.Time
	endTime   time.Time
}

// Configuration은 스토리지 설정이다.
// 실제 Jaeger: internal/storage/v2/memory/config.go의 Configuration
type Configuration struct {
	MaxTraces int // 최대 저장 Trace 수 (링 버퍼 크기)
}

// =============================================================================
// MemoryStore (Ring Buffer 기반)
// =============================================================================

// MemoryStore는 링 버퍼 기반의 인메모리 Trace 저장소이다.
// 실제 Jaeger 소스: internal/storage/v2/memory/tenant.go의 Tenant 구조체
//
//	type Tenant struct {
//	    mu     sync.RWMutex
//	    config *Configuration
//	    ids        map[pcommon.TraceID]int  // trace id → ring buffer 인덱스
//	    traces     []traceAndId             // 링 버퍼
//	    mostRecent int                      // 가장 최근 삽입 위치
//	    services   map[string]struct{}
//	    operations map[string]map[Operation]struct{}
//	}
type MemoryStore struct {
	mu     sync.RWMutex
	config Configuration

	// 링 버퍼: 고정 크기 배열 + 순환 인덱스
	ids        map[string]int     // traceID → traces[] 인덱스
	traces     []traceRecord      // 링 버퍼 (고정 크기)
	mostRecent int                // 가장 최근 삽입 위치

	// 인덱스: 서비스 및 오퍼레이션 빠른 조회용
	services   map[string]struct{}
	operations map[string]map[Operation]struct{}

	// 통계
	totalWrites  int
	totalEvicts  int
}

// NewMemoryStore는 새로운 인메모리 저장소를 생성한다.
// 실제 Jaeger: internal/storage/v2/memory/tenant.go의 newTenant()
func NewMemoryStore(cfg Configuration) (*MemoryStore, error) {
	if cfg.MaxTraces <= 0 {
		return nil, fmt.Errorf("max_traces는 0보다 커야 합니다 (현재: %d)", cfg.MaxTraces)
	}
	return &MemoryStore{
		config:     cfg,
		ids:        make(map[string]int),
		traces:     make([]traceRecord, cfg.MaxTraces),
		mostRecent: -1,
		services:   make(map[string]struct{}),
		operations: make(map[string]map[Operation]struct{}),
	}, nil
}

// WriteSpan은 Span을 저장소에 기록한다.
// 동일한 TraceID의 Span이 이미 존재하면 해당 Trace에 추가한다.
// 새로운 TraceID면 링 버퍼의 다음 위치에 저장하고, 기존 항목을 퇴거한다.
//
// 실제 Jaeger: internal/storage/v2/memory/tenant.go의 storeTraces()
// 핵심 로직:
//
//	if index, ok := t.ids[traceId]; ok {
//	    // 기존 Trace에 추가
//	} else {
//	    t.mostRecent = (t.mostRecent + 1) % t.config.MaxTraces
//	    if !t.traces[t.mostRecent].id.IsEmpty() {
//	        delete(t.ids, t.traces[t.mostRecent].id)  // 퇴거
//	    }
//	    t.ids[traceId] = t.mostRecent
//	    t.traces[t.mostRecent] = traceAndId{...}
//	}
func (s *MemoryStore) WriteSpan(span Span) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalWrites++

	// 서비스/오퍼레이션 인덱스 업데이트
	s.services[span.ServiceName] = struct{}{}
	if _, ok := s.operations[span.ServiceName]; !ok {
		s.operations[span.ServiceName] = make(map[Operation]struct{})
	}
	s.operations[span.ServiceName][Operation{
		Name:     span.OperationName,
		SpanKind: span.SpanKind,
	}] = struct{}{}

	// 기존 Trace에 Span 추가
	if index, ok := s.ids[span.TraceID]; ok {
		record := &s.traces[index]
		record.spans = append(record.spans, span)
		// 시간 범위 업데이트
		if span.StartTime.Before(record.startTime) {
			record.startTime = span.StartTime
		}
		endTime := span.StartTime.Add(span.Duration)
		if endTime.After(record.endTime) {
			record.endTime = endTime
		}
		return nil
	}

	// 새 Trace: 링 버퍼의 다음 위치에 저장
	s.mostRecent = (s.mostRecent + 1) % s.config.MaxTraces

	// 기존 항목 퇴거 (LRU)
	evicted := s.traces[s.mostRecent]
	if evicted.traceID != "" {
		delete(s.ids, evicted.traceID)
		s.totalEvicts++
	}

	// 새 레코드 저장
	endTime := span.StartTime.Add(span.Duration)
	s.ids[span.TraceID] = s.mostRecent
	s.traces[s.mostRecent] = traceRecord{
		traceID:   span.TraceID,
		spans:     []Span{span},
		startTime: span.StartTime,
		endTime:   endTime,
	}

	return nil
}

// GetTrace는 TraceID로 Trace를 조회한다.
func (s *MemoryStore) GetTrace(traceID string) ([]Span, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	index, ok := s.ids[traceID]
	if !ok {
		return nil, fmt.Errorf("trace %s not found", traceID)
	}
	record := s.traces[index]
	result := make([]Span, len(record.spans))
	copy(result, record.spans)
	return result, nil
}

// FindTraces는 쿼리 조건에 맞는 Trace 목록을 반환한다.
// mostRecent 위치에서 역순으로 탐색하여 최신 Trace부터 반환한다.
//
// 실제 Jaeger: internal/storage/v2/memory/tenant.go의 findTraceAndIds()
//
//	for i := range t.traces {
//	    index := (t.mostRecent - i + n) % n  // 최신부터 역순
//	    if traceById.id.IsEmpty() { break }   // 빈 슬롯 도달
//	    if validTrace(traceById.trace, query) {
//	        traceAndIds = append(traceAndIds, traceById)
//	    }
//	}
func (s *MemoryStore) FindTraces(query TraceQueryParams) ([][]Span, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	searchDepth := query.SearchDepth
	if searchDepth <= 0 || searchDepth > s.config.MaxTraces {
		searchDepth = s.config.MaxTraces
	}

	results := make([][]Span, 0)
	n := len(s.traces)

	for i := 0; i < n; i++ {
		if len(results) >= searchDepth {
			break
		}
		// 가장 최근 Trace부터 역순으로 탐색
		index := (s.mostRecent - i + n) % n
		record := s.traces[index]

		if record.traceID == "" {
			// 빈 슬롯: 링 버퍼가 아직 가득 차지 않은 영역
			break
		}

		if s.validTrace(record, query) {
			cp := make([]Span, len(record.spans))
			copy(cp, record.spans)
			results = append(results, cp)
		}
	}

	return results, nil
}

// validTrace는 Trace가 쿼리 조건을 만족하는지 검사한다.
// 실제 Jaeger: internal/storage/v2/memory/tenant.go의 validTrace()
func (s *MemoryStore) validTrace(record traceRecord, query TraceQueryParams) bool {
	for _, span := range record.spans {
		if s.validSpan(span, query) {
			return true
		}
	}
	return false
}

// validSpan은 개별 Span이 쿼리 조건을 만족하는지 검사한다.
// 실제 Jaeger: internal/storage/v2/memory/tenant.go의 validSpan()
func (s *MemoryStore) validSpan(span Span, query TraceQueryParams) bool {
	// 서비스 이름 필터
	if query.ServiceName != "" && span.ServiceName != query.ServiceName {
		return false
	}

	// 오퍼레이션 이름 필터
	if query.OperationName != "" && span.OperationName != query.OperationName {
		return false
	}

	// 시작 시간 범위 필터
	if !query.StartTimeMin.IsZero() && span.StartTime.Before(query.StartTimeMin) {
		return false
	}
	if !query.StartTimeMax.IsZero() && span.StartTime.After(query.StartTimeMax) {
		return false
	}

	// Duration 필터
	if query.DurationMin != 0 && span.Duration < query.DurationMin {
		return false
	}
	if query.DurationMax != 0 && span.Duration > query.DurationMax {
		return false
	}

	return true
}

// GetServices는 저장된 모든 서비스 이름을 반환한다.
// 실제 Jaeger: internal/storage/v2/memory/memory.go의 GetServices()
func (s *MemoryStore) GetServices() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	services := make([]string, 0, len(s.services))
	for svc := range s.services {
		services = append(services, svc)
	}
	sort.Strings(services)
	return services
}

// GetOperations는 특정 서비스의 오퍼레이션 목록을 반환한다.
// 실제 Jaeger: internal/storage/v2/memory/memory.go의 GetOperations()
func (s *MemoryStore) GetOperations(serviceName string) []Operation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ops, ok := s.operations[serviceName]
	if !ok {
		return nil
	}
	result := make([]Operation, 0, len(ops))
	for op := range ops {
		result = append(result, op)
	}
	return result
}

// Stats는 저장소 통계를 반환한다.
func (s *MemoryStore) Stats() (traceCount int, totalWrites int, totalEvicts int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ids), s.totalWrites, s.totalEvicts
}

// DumpRingBuffer는 링 버퍼 상태를 시각적으로 출력한다.
func (s *MemoryStore) DumpRingBuffer() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fmt.Printf("  링 버퍼 상태 (크기: %d, 현재 위치: %d)\n", s.config.MaxTraces, s.mostRecent)
	fmt.Println("  " + strings.Repeat("-", 60))

	for i := 0; i < s.config.MaxTraces; i++ {
		record := s.traces[i]
		marker := "  "
		if i == s.mostRecent {
			marker = "→ "
		}

		if record.traceID == "" {
			fmt.Printf("  %s[%d] (빈 슬롯)\n", marker, i)
		} else {
			displayID := record.traceID
			if len(displayID) > 12 {
				displayID = displayID[:12] + "..."
			}
			fmt.Printf("  %s[%d] TraceID=%s  spans=%d  시간범위=[%s ~ %s]\n",
				marker, i, displayID,
				len(record.spans),
				record.startTime.Format("15:04:05.000"),
				record.endTime.Format("15:04:05.000"),
			)
		}
	}
	fmt.Println()
}

// =============================================================================
// 샘플 데이터 생성
// =============================================================================

func generateTraceID() string {
	return fmt.Sprintf("%032x", rand.Int63())
}

func generateSpanID() string {
	return fmt.Sprintf("%016x", rand.Int63())
}

type sampleService struct {
	name       string
	operations []string
	spanKinds  []string
}

var sampleServices = []sampleService{
	{"frontend", []string{"HTTP GET /", "HTTP GET /api/users", "HTTP POST /api/orders"}, []string{"server"}},
	{"user-service", []string{"gRPC GetUser", "gRPC ListUsers", "DB SELECT"}, []string{"server", "client"}},
	{"order-service", []string{"gRPC CreateOrder", "gRPC GetOrder", "DB INSERT", "DB SELECT"}, []string{"server", "client"}},
	{"payment-service", []string{"gRPC ProcessPayment", "HTTP POST /charge"}, []string{"server", "client"}},
	{"inventory-service", []string{"gRPC CheckStock", "gRPC ReserveItem", "DB UPDATE"}, []string{"server", "client"}},
	{"notification-service", []string{"SendEmail", "SendSMS", "PushNotification"}, []string{"producer", "consumer"}},
}

func generateSpansForTrace(traceID string, baseTime time.Time) []Span {
	spanCount := 2 + rand.Intn(4) // 2~5개 Span
	spans := make([]Span, 0, spanCount)

	for i := 0; i < spanCount; i++ {
		svc := sampleServices[rand.Intn(len(sampleServices))]
		op := svc.operations[rand.Intn(len(svc.operations))]
		kind := svc.spanKinds[rand.Intn(len(svc.spanKinds))]

		spans = append(spans, Span{
			TraceID:       traceID,
			SpanID:        generateSpanID(),
			OperationName: op,
			ServiceName:   svc.name,
			StartTime:     baseTime.Add(time.Duration(i*10+rand.Intn(20)) * time.Millisecond),
			Duration:      time.Duration(5+rand.Intn(200)) * time.Millisecond,
			SpanKind:      kind,
			Tags:          map[string]string{"component": svc.name},
		})
	}
	return spans
}

// =============================================================================
// 동시성 테스트
// =============================================================================

func concurrencyTest(store *MemoryStore) {
	fmt.Println("=== 동시성 테스트 (10 goroutine x 5 writes) ===")
	fmt.Println()

	var wg sync.WaitGroup
	baseTime := time.Now()
	errorCount := 0
	var mu sync.Mutex

	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for w := 0; w < 5; w++ {
				traceID := generateTraceID()
				span := Span{
					TraceID:       traceID,
					SpanID:        generateSpanID(),
					OperationName: fmt.Sprintf("concurrent-op-%d-%d", goroutineID, w),
					ServiceName:   fmt.Sprintf("concurrent-svc-%d", goroutineID%3),
					StartTime:     baseTime.Add(time.Duration(w) * time.Millisecond),
					Duration:      time.Duration(10+rand.Intn(50)) * time.Millisecond,
					SpanKind:      "server",
				}
				if err := store.WriteSpan(span); err != nil {
					mu.Lock()
					errorCount++
					mu.Unlock()
				}
			}
		}(g)
	}

	wg.Wait()

	traceCount, totalWrites, totalEvicts := store.Stats()
	fmt.Printf("  동시 쓰기 완료: %d goroutines x 5 writes = %d writes\n", 10, totalWrites)
	fmt.Printf("  에러 수: %d\n", errorCount)
	fmt.Printf("  현재 저장된 Trace: %d (max: %d)\n", traceCount, store.config.MaxTraces)
	fmt.Printf("  퇴거된 Trace: %d\n", totalEvicts)
	fmt.Println()
}

// =============================================================================
// 메인 실행
// =============================================================================

func main() {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jaeger In-Memory Store 시뮬레이션                           ║")
	fmt.Println("║  (internal/storage/v2/memory/tenant.go 기반)                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// ─── 1. 링 버퍼 기반 저장 구조 설명 ───
	fmt.Println("=== 링 버퍼 기반 저장 구조 ===")
	fmt.Println()
	fmt.Println("  Jaeger 인메모리 스토리지는 링 버퍼로 최대 Trace 수를 제한한다.")
	fmt.Println("  링 버퍼가 가득 차면 가장 오래된 Trace를 퇴거(evict)한다.")
	fmt.Println()
	fmt.Println("  ┌───┬───┬───┬───┬───┐")
	fmt.Println("  │ 0 │ 1 │ 2 │ 3 │ 4 │  ← traces[] 배열 (MaxTraces=5)")
	fmt.Println("  └───┴───┴───┴───┴───┘")
	fmt.Println("              ↑")
	fmt.Println("          mostRecent")
	fmt.Println()
	fmt.Println("  삽입: mostRecent = (mostRecent + 1) % MaxTraces")
	fmt.Println("  퇴거: traces[mostRecent]의 기존 항목 삭제")
	fmt.Println("  조회: mostRecent에서 역순으로 탐색 (최신 → 오래된 순)")
	fmt.Println()

	// ─── 2. 작은 링 버퍼로 퇴거 동작 시연 ───
	fmt.Println("=== 링 버퍼 퇴거(Eviction) 시연 (MaxTraces=5) ===")
	fmt.Println()

	store, err := NewMemoryStore(Configuration{MaxTraces: 5})
	if err != nil {
		fmt.Printf("스토어 생성 실패: %v\n", err)
		return
	}

	baseTime := time.Now()
	traceIDs := make([]string, 8)

	// 8개 Trace 저장 (링 버퍼 크기 5를 초과)
	for i := 0; i < 8; i++ {
		traceIDs[i] = generateTraceID()
		spans := generateSpansForTrace(traceIDs[i], baseTime.Add(time.Duration(i)*time.Second))

		for _, span := range spans {
			store.WriteSpan(span)
		}

		displayID := traceIDs[i][:12] + "..."
		fmt.Printf("  [%d] Trace 저장: %s (%d spans)\n", i+1, displayID, len(spans))

		if i >= 4 {
			fmt.Printf("      → 링 버퍼 가득 참: 가장 오래된 Trace 퇴거!\n")
		}
	}
	fmt.Println()

	// 링 버퍼 상태 출력
	store.DumpRingBuffer()

	// 퇴거된 Trace 접근 시도
	fmt.Println("  퇴거된 Trace 접근 시도:")
	for i := 0; i < 8; i++ {
		_, err := store.GetTrace(traceIDs[i])
		displayID := traceIDs[i][:12] + "..."
		if err != nil {
			fmt.Printf("    Trace #%d (%s): 퇴거됨 (not found)\n", i+1, displayID)
		} else {
			fmt.Printf("    Trace #%d (%s): 존재함\n", i+1, displayID)
		}
	}

	traceCount, totalWrites, totalEvicts := store.Stats()
	fmt.Println()
	fmt.Printf("  통계: 저장된 Trace=%d, 총 쓰기=%d, 퇴거=%d\n",
		traceCount, totalWrites, totalEvicts)
	fmt.Println()

	// ─── 3. 서비스/오퍼레이션 인덱싱 ───
	fmt.Println("=== 서비스/오퍼레이션 인덱싱 ===")
	fmt.Println()

	services := store.GetServices()
	fmt.Printf("  등록된 서비스 (%d개): %s\n", len(services), strings.Join(services, ", "))
	fmt.Println()

	for _, svc := range services {
		ops := store.GetOperations(svc)
		opNames := make([]string, len(ops))
		for i, op := range ops {
			if op.SpanKind != "" {
				opNames[i] = fmt.Sprintf("%s(%s)", op.Name, op.SpanKind)
			} else {
				opNames[i] = op.Name
			}
		}
		fmt.Printf("  %s:\n", svc)
		for _, name := range opNames {
			fmt.Printf("    - %s\n", name)
		}
	}
	fmt.Println()

	// ─── 4. FindTraces 쿼리 테스트 ───
	fmt.Println("=== FindTraces 쿼리 테스트 ===")
	fmt.Println()

	// 더 큰 스토어로 교체하여 쿼리 테스트
	queryStore, _ := NewMemoryStore(Configuration{MaxTraces: 100})

	// 다양한 서비스의 Trace 20개 생성
	queryBase := time.Now()
	for i := 0; i < 20; i++ {
		traceID := generateTraceID()
		spans := generateSpansForTrace(traceID, queryBase.Add(time.Duration(i)*100*time.Millisecond))
		for _, span := range spans {
			queryStore.WriteSpan(span)
		}
	}

	qTraceCount, _, _ := queryStore.Stats()
	fmt.Printf("  테스트 데이터: %d traces 저장됨\n", qTraceCount)
	fmt.Println()

	// 쿼리 1: 서비스 이름으로 검색
	fmt.Println("  쿼리 1: ServiceName = 'frontend'")
	results, _ := queryStore.FindTraces(TraceQueryParams{
		ServiceName: "frontend",
		SearchDepth: 10,
	})
	fmt.Printf("    결과: %d traces\n", len(results))
	for i, trace := range results {
		if i >= 3 {
			fmt.Printf("    ... 외 %d개 Trace\n", len(results)-3)
			break
		}
		svcSet := make(map[string]int)
		for _, s := range trace {
			svcSet[s.ServiceName]++
		}
		parts := []string{}
		for svc, cnt := range svcSet {
			parts = append(parts, fmt.Sprintf("%s(%d)", svc, cnt))
		}
		fmt.Printf("    [%d] %d spans: %s\n", i+1, len(trace), strings.Join(parts, ", "))
	}
	fmt.Println()

	// 쿼리 2: 서비스 + 오퍼레이션으로 검색
	fmt.Println("  쿼리 2: ServiceName = 'order-service', OperationName = 'gRPC CreateOrder'")
	results, _ = queryStore.FindTraces(TraceQueryParams{
		ServiceName:   "order-service",
		OperationName: "gRPC CreateOrder",
		SearchDepth:   10,
	})
	fmt.Printf("    결과: %d traces\n", len(results))
	fmt.Println()

	// 쿼리 3: 시간 범위로 검색
	midTime := queryBase.Add(1 * time.Second)
	fmt.Printf("  쿼리 3: StartTimeMin = %s (최근 Trace만)\n", midTime.Format("15:04:05.000"))
	results, _ = queryStore.FindTraces(TraceQueryParams{
		StartTimeMin: midTime,
		SearchDepth:  10,
	})
	fmt.Printf("    결과: %d traces\n", len(results))
	fmt.Println()

	// 쿼리 4: SearchDepth 제한
	fmt.Println("  쿼리 4: SearchDepth = 3 (최대 3개만 반환)")
	results, _ = queryStore.FindTraces(TraceQueryParams{
		SearchDepth: 3,
	})
	fmt.Printf("    결과: %d traces (SearchDepth=3으로 제한됨)\n", len(results))
	fmt.Println()

	// ─── 5. 동시성 테스트 ───
	concurrencyStore, _ := NewMemoryStore(Configuration{MaxTraces: 30})
	concurrencyTest(concurrencyStore)

	// ─── 6. 동일 TraceID에 Span 추가 ───
	fmt.Println("=== 동일 TraceID에 Span 추가 테스트 ===")
	fmt.Println()

	appendStore, _ := NewMemoryStore(Configuration{MaxTraces: 10})
	sharedTraceID := generateTraceID()
	displayID := sharedTraceID[:16] + "..."

	// 같은 TraceID로 여러 Span 추가
	for i := 0; i < 5; i++ {
		svc := sampleServices[i%len(sampleServices)]
		span := Span{
			TraceID:       sharedTraceID,
			SpanID:        generateSpanID(),
			OperationName: svc.operations[0],
			ServiceName:   svc.name,
			StartTime:     baseTime.Add(time.Duration(i*20) * time.Millisecond),
			Duration:      time.Duration(10+i*30) * time.Millisecond,
			SpanKind:      svc.spanKinds[0],
		}
		appendStore.WriteSpan(span)
		fmt.Printf("  [%d] Span 추가: %s::%s\n", i+1, svc.name, svc.operations[0])
	}

	spans, _ := appendStore.GetTrace(sharedTraceID)
	fmt.Printf("\n  TraceID: %s\n", displayID)
	fmt.Printf("  저장된 Span 수: %d (동일 TraceID에 누적됨)\n", len(spans))
	for i, span := range spans {
		fmt.Printf("    [%d] %s::%s (%s)\n", i+1, span.ServiceName, span.OperationName, span.Duration)
	}

	appendCount, _, _ := appendStore.Stats()
	fmt.Printf("\n  Trace 수: %d (5개 Span이 1개 Trace에 속함)\n", appendCount)
	fmt.Println()

	// ─── 요약 ───
	fmt.Println("=== 요약: Jaeger In-Memory Store 핵심 설계 ===")
	fmt.Println()
	fmt.Println("  1. 링 버퍼 기반 저장:")
	fmt.Println("     - traces[]는 고정 크기 배열, mostRecent 인덱스가 순환")
	fmt.Println("     - MaxTraces 초과 시 가장 오래된 Trace 자동 퇴거")
	fmt.Println("     - O(1) 삽입, 메모리 사용량 예측 가능")
	fmt.Println()
	fmt.Println("  2. 인덱싱:")
	fmt.Println("     - ids map: traceID → 링 버퍼 인덱스 (O(1) 조회)")
	fmt.Println("     - services map: 전체 서비스 목록 (GetServices)")
	fmt.Println("     - operations map: 서비스별 오퍼레이션 (GetOperations)")
	fmt.Println()
	fmt.Println("  3. 쿼리 (FindTraces):")
	fmt.Println("     - mostRecent에서 역순 탐색 (최신 Trace 우선)")
	fmt.Println("     - SearchDepth로 탐색 범위 제한")
	fmt.Println("     - validTrace → validSpan 조건 필터링")
	fmt.Println()
	fmt.Println("  4. 동시성:")
	fmt.Println("     - sync.RWMutex: 읽기는 공유, 쓰기는 배타적")
	fmt.Println("     - 실제 Jaeger는 멀티테넌트를 위해 Store → Tenant 2단계 잠금")
}
