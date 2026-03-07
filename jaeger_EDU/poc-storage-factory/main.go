package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Jaeger Storage Factory 패턴 시뮬레이션
// =============================================================================
// Jaeger는 다양한 스토리지 백엔드(Cassandra, Elasticsearch, Memory, Badger 등)를
// 일관된 인터페이스로 추상화하기 위해 Factory 패턴을 사용한다.
//
// 실제 소스 경로:
// - internal/storage/v2/api/tracestore/factory.go : Factory 인터페이스 정의
// - internal/storage/v2/api/tracestore/reader.go  : Reader 인터페이스 정의
// - internal/storage/v2/api/tracestore/writer.go  : Writer 인터페이스 정의
// - internal/storage/v2/memory/factory.go         : Memory Factory 구현
// - internal/storage/v2/cassandra/factory.go      : Cassandra Factory 구현
// - internal/storage/v2/elasticsearch/factory.go  : Elasticsearch Factory 구현
// - docs/adr/003-lazy-storage-factory-initialization.md : 지연 초기화 ADR
//
// 핵심 설계:
// 1. Factory 인터페이스로 백엔드 독립적인 코드 작성 가능
// 2. 지연 초기화(Lazy Initialization)로 실제 사용 시점에 팩토리 생성
// 3. Config 기반 백엔드 선택으로 런타임에 스토리지 전환 가능
// =============================================================================

// =============================================================================
// 데이터 모델 (간소화)
// =============================================================================

// Span은 저장 및 조회의 기본 단위이다.
type Span struct {
	TraceID       string            `json:"traceID"`
	SpanID        string            `json:"spanID"`
	OperationName string            `json:"operationName"`
	ServiceName   string            `json:"serviceName"`
	StartTime     time.Time         `json:"startTime"`
	Duration      time.Duration     `json:"duration"`
	Tags          map[string]string `json:"tags,omitempty"`
}

// TraceQueryParams는 Trace 검색 조건이다.
// 실제 Jaeger: internal/storage/v2/api/tracestore/reader.go의 TraceQueryParams
type TraceQueryParams struct {
	ServiceName   string
	OperationName string
	StartTimeMin  time.Time
	StartTimeMax  time.Time
}

// Operation은 서비스의 오퍼레이션 정보이다.
// 실제 Jaeger: internal/storage/v2/api/tracestore/reader.go의 Operation
type Operation struct {
	Name     string
	SpanKind string
}

// =============================================================================
// 스토리지 인터페이스
// =============================================================================

// Reader는 스토리지에서 데이터를 읽는 인터페이스이다.
// 실제 Jaeger 소스: internal/storage/v2/api/tracestore/reader.go
//
//	type Reader interface {
//	    GetTraces(ctx, traceIDs...) iter.Seq2[[]ptrace.Traces, error]
//	    GetServices(ctx) ([]string, error)
//	    GetOperations(ctx, query) ([]Operation, error)
//	    FindTraces(ctx, query) iter.Seq2[[]ptrace.Traces, error]
//	}
type Reader interface {
	GetTrace(traceID string) ([]Span, error)
	FindTraces(query TraceQueryParams) ([][]Span, error)
	GetServices() ([]string, error)
	GetOperations(serviceName string) ([]Operation, error)
}

// Writer는 스토리지에 데이터를 기록하는 인터페이스이다.
// 실제 Jaeger 소스: internal/storage/v2/api/tracestore/writer.go
//
//	type Writer interface {
//	    WriteTraces(ctx, td ptrace.Traces) error
//	}
type Writer interface {
	WriteSpan(span Span) error
}

// Factory는 Reader와 Writer를 생성하는 팩토리 인터페이스이다.
// 실제 Jaeger 소스: internal/storage/v2/api/tracestore/factory.go
//
//	type Factory interface {
//	    CreateTraceReader() (Reader, error)
//	    CreateTraceWriter() (Writer, error)
//	}
type Factory interface {
	CreateReader() (Reader, error)
	CreateWriter() (Writer, error)
	Close() error
}

// =============================================================================
// Memory Factory 구현
// =============================================================================

// MemoryStore는 인메모리 스토리지이다.
// 실제 Jaeger: internal/storage/v2/memory/memory.go의 Store
type MemoryStore struct {
	mu     sync.RWMutex
	traces map[string][]Span
}

func (s *MemoryStore) WriteSpan(span Span) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.traces[span.TraceID] = append(s.traces[span.TraceID], span)
	return nil
}

func (s *MemoryStore) GetTrace(traceID string) ([]Span, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	spans, ok := s.traces[traceID]
	if !ok {
		return nil, fmt.Errorf("trace %s not found", traceID)
	}
	result := make([]Span, len(spans))
	copy(result, spans)
	return result, nil
}

func (s *MemoryStore) FindTraces(query TraceQueryParams) ([][]Span, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var results [][]Span
	for _, spans := range s.traces {
		match := false
		for _, span := range spans {
			if query.ServiceName != "" && span.ServiceName != query.ServiceName {
				continue
			}
			if query.OperationName != "" && span.OperationName != query.OperationName {
				continue
			}
			match = true
			break
		}
		if match {
			cp := make([]Span, len(spans))
			copy(cp, spans)
			results = append(results, cp)
		}
	}
	return results, nil
}

func (s *MemoryStore) GetServices() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	svcSet := make(map[string]struct{})
	for _, spans := range s.traces {
		for _, span := range spans {
			svcSet[span.ServiceName] = struct{}{}
		}
	}
	services := make([]string, 0, len(svcSet))
	for svc := range svcSet {
		services = append(services, svc)
	}
	return services, nil
}

func (s *MemoryStore) GetOperations(serviceName string) ([]Operation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	opSet := make(map[string]struct{})
	for _, spans := range s.traces {
		for _, span := range spans {
			if span.ServiceName == serviceName {
				opSet[span.OperationName] = struct{}{}
			}
		}
	}
	ops := make([]Operation, 0, len(opSet))
	for op := range opSet {
		ops = append(ops, Operation{Name: op})
	}
	return ops, nil
}

// MemoryFactory는 인메모리 스토리지 팩토리이다.
// 실제 Jaeger 소스: internal/storage/v2/memory/factory.go
//
//	type Factory struct {
//	    store          *Store
//	    metricsFactory metrics.Factory
//	}
type MemoryFactory struct {
	store *MemoryStore
}

func NewMemoryFactory() *MemoryFactory {
	fmt.Println("  [MemoryFactory] 팩토리 생성됨 (인메모리 스토리지)")
	return &MemoryFactory{
		store: &MemoryStore{
			traces: make(map[string][]Span),
		},
	}
}

func (f *MemoryFactory) CreateReader() (Reader, error) {
	fmt.Println("  [MemoryFactory] Reader 생성됨")
	return f.store, nil
}

func (f *MemoryFactory) CreateWriter() (Writer, error) {
	fmt.Println("  [MemoryFactory] Writer 생성됨")
	return f.store, nil
}

func (f *MemoryFactory) Close() error {
	fmt.Println("  [MemoryFactory] 팩토리 종료됨")
	return nil
}

// =============================================================================
// File Factory 구현
// =============================================================================

// FileStore는 파일 기반 스토리지이다.
// 각 Trace를 별도의 JSON 파일로 저장한다.
// 실제 Jaeger의 Badger Factory가 디스크 기반 스토리지인 것을 참고했다.
type FileStore struct {
	mu      sync.RWMutex
	baseDir string
	// 인덱스: traceID → 파일 경로, 서비스 → 오퍼레이션
	traceIndex   map[string]string
	serviceIndex map[string]map[string]struct{}
}

func NewFileStore(baseDir string) (*FileStore, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("디렉토리 생성 실패: %w", err)
	}
	return &FileStore{
		baseDir:      baseDir,
		traceIndex:   make(map[string]string),
		serviceIndex: make(map[string]map[string]struct{}),
	}, nil
}

func (s *FileStore) WriteSpan(span Span) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 기존 Span 로드
	filePath := filepath.Join(s.baseDir, span.TraceID+".json")
	var spans []Span

	if data, err := os.ReadFile(filePath); err == nil {
		if err := json.Unmarshal(data, &spans); err != nil {
			return fmt.Errorf("JSON 파싱 실패: %w", err)
		}
	}

	spans = append(spans, span)

	// JSON 파일로 저장
	data, err := json.MarshalIndent(spans, "", "  ")
	if err != nil {
		return fmt.Errorf("JSON 직렬화 실패: %w", err)
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return fmt.Errorf("파일 저장 실패: %w", err)
	}

	// 인덱스 업데이트
	s.traceIndex[span.TraceID] = filePath
	if _, ok := s.serviceIndex[span.ServiceName]; !ok {
		s.serviceIndex[span.ServiceName] = make(map[string]struct{})
	}
	s.serviceIndex[span.ServiceName][span.OperationName] = struct{}{}

	return nil
}

func (s *FileStore) GetTrace(traceID string) ([]Span, error) {
	s.mu.RLock()
	filePath, ok := s.traceIndex[traceID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("trace %s not found", traceID)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("파일 읽기 실패: %w", err)
	}
	var spans []Span
	if err := json.Unmarshal(data, &spans); err != nil {
		return nil, fmt.Errorf("JSON 파싱 실패: %w", err)
	}
	return spans, nil
}

func (s *FileStore) FindTraces(query TraceQueryParams) ([][]Span, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results [][]Span
	for traceID := range s.traceIndex {
		spans, err := s.GetTrace(traceID)
		if err != nil {
			continue
		}
		match := false
		for _, span := range spans {
			if query.ServiceName != "" && span.ServiceName != query.ServiceName {
				continue
			}
			if query.OperationName != "" && span.OperationName != query.OperationName {
				continue
			}
			match = true
			break
		}
		if match {
			results = append(results, spans)
		}
	}
	return results, nil
}

func (s *FileStore) GetServices() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	services := make([]string, 0, len(s.serviceIndex))
	for svc := range s.serviceIndex {
		services = append(services, svc)
	}
	return services, nil
}

func (s *FileStore) GetOperations(serviceName string) ([]Operation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ops, ok := s.serviceIndex[serviceName]
	if !ok {
		return nil, nil
	}
	result := make([]Operation, 0, len(ops))
	for op := range ops {
		result = append(result, Operation{Name: op})
	}
	return result, nil
}

// FileFactory는 파일 기반 스토리지 팩토리이다.
type FileFactory struct {
	store   *FileStore
	baseDir string
}

func NewFileFactory(baseDir string) (*FileFactory, error) {
	store, err := NewFileStore(baseDir)
	if err != nil {
		return nil, err
	}
	fmt.Printf("  [FileFactory] 팩토리 생성됨 (파일 스토리지: %s)\n", baseDir)
	return &FileFactory{
		store:   store,
		baseDir: baseDir,
	}, nil
}

func (f *FileFactory) CreateReader() (Reader, error) {
	fmt.Println("  [FileFactory] Reader 생성됨")
	return f.store, nil
}

func (f *FileFactory) CreateWriter() (Writer, error) {
	fmt.Println("  [FileFactory] Writer 생성됨")
	return f.store, nil
}

func (f *FileFactory) Close() error {
	fmt.Println("  [FileFactory] 팩토리 종료됨")
	// 임시 파일 정리
	os.RemoveAll(f.baseDir)
	fmt.Printf("  [FileFactory] 임시 디렉토리 삭제됨: %s\n", f.baseDir)
	return nil
}

// =============================================================================
// Lazy Factory (지연 초기화)
// =============================================================================

// LazyFactory는 실제 팩토리를 첫 접근 시점에 생성하는 래퍼이다.
// Jaeger의 ADR-003(docs/adr/003-lazy-storage-factory-initialization.md)에서
// 제안한 지연 초기화 패턴을 구현한다.
//
// 배경: Jaeger는 여러 스토리지 백엔드를 지원하며, 실제로 사용되지 않는 백엔드까지
// 초기화하면 불필요한 리소스 낭비와 초기화 실패 위험이 있다.
// 지연 초기화를 통해 실제 사용 시점에만 팩토리를 생성한다.
type LazyFactory struct {
	mu      sync.Mutex
	factory Factory
	creator func() (Factory, error)
}

func NewLazyFactory(creator func() (Factory, error)) *LazyFactory {
	fmt.Println("  [LazyFactory] 래퍼 생성됨 (실제 팩토리는 아직 초기화 안 됨)")
	return &LazyFactory{
		creator: creator,
	}
}

func (lf *LazyFactory) getOrCreate() (Factory, error) {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	if lf.factory == nil {
		fmt.Println("  [LazyFactory] 첫 접근 - 실제 팩토리 초기화 시작")
		factory, err := lf.creator()
		if err != nil {
			return nil, fmt.Errorf("팩토리 초기화 실패: %w", err)
		}
		lf.factory = factory
		fmt.Println("  [LazyFactory] 실제 팩토리 초기화 완료")
	} else {
		fmt.Println("  [LazyFactory] 캐시된 팩토리 반환")
	}
	return lf.factory, nil
}

func (lf *LazyFactory) CreateReader() (Reader, error) {
	f, err := lf.getOrCreate()
	if err != nil {
		return nil, err
	}
	return f.CreateReader()
}

func (lf *LazyFactory) CreateWriter() (Writer, error) {
	f, err := lf.getOrCreate()
	if err != nil {
		return nil, err
	}
	return f.CreateWriter()
}

func (lf *LazyFactory) Close() error {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	if lf.factory != nil {
		return lf.factory.Close()
	}
	return nil
}

// =============================================================================
// Config 기반 팩토리 선택
// =============================================================================

// StorageConfig는 스토리지 설정이다.
type StorageConfig struct {
	BackendType string // "memory" 또는 "file"
	FilePath    string // file 백엔드 전용
}

// CreateFactory는 설정에 따라 적절한 팩토리를 생성한다.
// 실제 Jaeger에서는 internal/storage/v2 아래의 각 백엔드별 factory.go가 이 역할을 한다.
func CreateFactory(cfg StorageConfig) (Factory, error) {
	switch cfg.BackendType {
	case "memory":
		return NewMemoryFactory(), nil
	case "file":
		if cfg.FilePath == "" {
			cfg.FilePath = filepath.Join(os.TempDir(), "jaeger-poc-storage")
		}
		return NewFileFactory(cfg.FilePath)
	default:
		return nil, fmt.Errorf("지원하지 않는 백엔드: %s", cfg.BackendType)
	}
}

// =============================================================================
// 데모 헬퍼
// =============================================================================

func generateSampleSpans(traceID string) []Span {
	baseTime := time.Now()
	return []Span{
		{
			TraceID: traceID, SpanID: fmt.Sprintf("%016x", rand.Int63()),
			OperationName: "HTTP GET /api/orders", ServiceName: "frontend",
			StartTime: baseTime, Duration: 200 * time.Millisecond,
			Tags: map[string]string{"http.method": "GET", "http.status_code": "200"},
		},
		{
			TraceID: traceID, SpanID: fmt.Sprintf("%016x", rand.Int63()),
			OperationName: "gRPC GetOrder", ServiceName: "order-service",
			StartTime: baseTime.Add(10 * time.Millisecond), Duration: 150 * time.Millisecond,
			Tags: map[string]string{"rpc.system": "grpc"},
		},
		{
			TraceID: traceID, SpanID: fmt.Sprintf("%016x", rand.Int63()),
			OperationName: "DB SELECT orders", ServiceName: "order-service",
			StartTime: baseTime.Add(20 * time.Millisecond), Duration: 50 * time.Millisecond,
			Tags: map[string]string{"db.system": "postgresql"},
		},
	}
}

// testFactory는 주어진 팩토리로 쓰기/읽기 기능을 테스트한다.
func testFactory(factory Factory, label string) {
	fmt.Printf("\n--- %s: 쓰기/읽기 테스트 ---\n", label)

	// Writer 생성 및 Span 쓰기
	writer, err := factory.CreateWriter()
	if err != nil {
		fmt.Printf("  Writer 생성 실패: %v\n", err)
		return
	}

	traceID := fmt.Sprintf("%032x", rand.Int63())
	spans := generateSampleSpans(traceID)
	for _, span := range spans {
		if err := writer.WriteSpan(span); err != nil {
			fmt.Printf("  Span 쓰기 실패: %v\n", err)
			return
		}
	}
	fmt.Printf("  %d개 Span 저장 완료 (TraceID: %s...)\n", len(spans), traceID[:16])

	// Reader 생성 및 읽기
	reader, err := factory.CreateReader()
	if err != nil {
		fmt.Printf("  Reader 생성 실패: %v\n", err)
		return
	}

	// GetTrace 테스트
	readSpans, err := reader.GetTrace(traceID)
	if err != nil {
		fmt.Printf("  GetTrace 실패: %v\n", err)
		return
	}
	fmt.Printf("  GetTrace 결과: %d개 Span\n", len(readSpans))

	// GetServices 테스트
	services, err := reader.GetServices()
	if err != nil {
		fmt.Printf("  GetServices 실패: %v\n", err)
		return
	}
	fmt.Printf("  등록된 서비스: %s\n", strings.Join(services, ", "))

	// GetOperations 테스트
	for _, svc := range services {
		ops, err := reader.GetOperations(svc)
		if err != nil {
			continue
		}
		opNames := make([]string, len(ops))
		for i, op := range ops {
			opNames[i] = op.Name
		}
		fmt.Printf("  %s 오퍼레이션: %s\n", svc, strings.Join(opNames, ", "))
	}

	// FindTraces 테스트
	results, err := reader.FindTraces(TraceQueryParams{ServiceName: "order-service"})
	if err != nil {
		fmt.Printf("  FindTraces 실패: %v\n", err)
		return
	}
	fmt.Printf("  FindTraces(service=order-service): %d개 Trace\n", len(results))
}

// =============================================================================
// 메인 실행
// =============================================================================

func main() {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jaeger Storage Factory 패턴 시뮬레이션                       ║")
	fmt.Println("║  (internal/storage/v2/api/tracestore/factory.go 기반)       ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// ─── 1. Factory 패턴 구조 설명 ───
	fmt.Println("=== Factory 패턴 구조 ===")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────┐")
	fmt.Println("  │ Factory interface                                   │")
	fmt.Println("  │   CreateReader() (Reader, error)                    │")
	fmt.Println("  │   CreateWriter() (Writer, error)                    │")
	fmt.Println("  └──────────┬──────────────────────┬──────────────────┘")
	fmt.Println("             │                      │")
	fmt.Println("  ┌──────────▼────────┐  ┌──────────▼────────┐")
	fmt.Println("  │ MemoryFactory     │  │ FileFactory       │")
	fmt.Println("  │  (인메모리 저장)    │  │  (파일 기반 저장)  │")
	fmt.Println("  └───────────────────┘  └───────────────────┘")
	fmt.Println()
	fmt.Println("  ┌────────────────────────────────────────────┐")
	fmt.Println("  │ LazyFactory (지연 초기화 래퍼)               │")
	fmt.Println("  │   첫 접근 시 실제 Factory 생성               │")
	fmt.Println("  │   이후 캐시된 Factory 반환                   │")
	fmt.Println("  └────────────────────────────────────────────┘")
	fmt.Println()

	// ─── 2. Memory Factory 테스트 ───
	fmt.Println("=== 1. Memory Factory 테스트 ===")
	memConfig := StorageConfig{BackendType: "memory"}
	memFactory, err := CreateFactory(memConfig)
	if err != nil {
		fmt.Printf("팩토리 생성 실패: %v\n", err)
		return
	}
	testFactory(memFactory, "MemoryFactory")
	memFactory.Close()
	fmt.Println()

	// ─── 3. File Factory 테스트 ───
	fmt.Println("=== 2. File Factory 테스트 ===")
	tmpDir := filepath.Join(os.TempDir(), fmt.Sprintf("jaeger-poc-%d", time.Now().UnixNano()))
	fileConfig := StorageConfig{BackendType: "file", FilePath: tmpDir}
	fileFactory, err := CreateFactory(fileConfig)
	if err != nil {
		fmt.Printf("팩토리 생성 실패: %v\n", err)
		return
	}
	testFactory(fileFactory, "FileFactory")
	fileFactory.Close()
	fmt.Println()

	// ─── 4. Lazy Factory 테스트 ───
	fmt.Println("=== 3. Lazy Factory (지연 초기화) 테스트 ===")
	fmt.Println()
	fmt.Println("  지연 초기화 래퍼 생성:")
	lazyFactory := NewLazyFactory(func() (Factory, error) {
		return NewMemoryFactory(), nil
	})
	fmt.Println()

	fmt.Println("  첫 번째 CreateWriter 호출 (팩토리 초기화 발생):")
	_, err = lazyFactory.CreateWriter()
	if err != nil {
		fmt.Printf("  실패: %v\n", err)
		return
	}
	fmt.Println()

	fmt.Println("  두 번째 CreateReader 호출 (캐시된 팩토리 사용):")
	_, err = lazyFactory.CreateReader()
	if err != nil {
		fmt.Printf("  실패: %v\n", err)
		return
	}
	fmt.Println()

	fmt.Println("  세 번째 CreateWriter 호출 (캐시된 팩토리 사용):")
	_, err = lazyFactory.CreateWriter()
	if err != nil {
		fmt.Printf("  실패: %v\n", err)
		return
	}
	lazyFactory.Close()
	fmt.Println()

	// ─── 5. Config 기반 백엔드 전환 데모 ───
	fmt.Println("=== 4. Config 기반 백엔드 전환 데모 ===")
	fmt.Println()

	configs := []StorageConfig{
		{BackendType: "memory"},
		{BackendType: "file", FilePath: filepath.Join(os.TempDir(), fmt.Sprintf("jaeger-poc-switch-%d", time.Now().UnixNano()))},
	}

	for _, cfg := range configs {
		fmt.Printf("  설정: BackendType=%q\n", cfg.BackendType)
		factory, err := CreateFactory(cfg)
		if err != nil {
			fmt.Printf("  팩토리 생성 실패: %v\n", err)
			continue
		}

		writer, _ := factory.CreateWriter()
		reader, _ := factory.CreateReader()

		// 쓰기
		span := Span{
			TraceID:       fmt.Sprintf("%032x", rand.Int63()),
			SpanID:        fmt.Sprintf("%016x", rand.Int63()),
			OperationName: "test-op",
			ServiceName:   "test-service",
			StartTime:     time.Now(),
			Duration:      100 * time.Millisecond,
		}
		writer.WriteSpan(span)

		// 읽기
		services, _ := reader.GetServices()
		fmt.Printf("  저장된 서비스: %s\n", strings.Join(services, ", "))
		fmt.Printf("  백엔드 %q 정상 동작 확인 완료\n", cfg.BackendType)

		factory.Close()
		fmt.Println()
	}

	// ─── 6. 잘못된 백엔드 처리 ───
	fmt.Println("=== 5. 에러 처리: 지원하지 않는 백엔드 ===")
	_, err = CreateFactory(StorageConfig{BackendType: "redis"})
	if err != nil {
		fmt.Printf("  예상된 에러: %v\n", err)
	}
	fmt.Println()

	// ─── 요약 ───
	fmt.Println("=== 요약 ===")
	fmt.Println()
	fmt.Println("  Jaeger Storage Factory 패턴의 핵심 설계:")
	fmt.Println()
	fmt.Println("  1. 인터페이스 추상화: Factory/Reader/Writer 인터페이스로")
	fmt.Println("     백엔드 독립적인 코드 작성 가능")
	fmt.Println()
	fmt.Println("  2. 지연 초기화(Lazy Init): 실제 사용 시점에 팩토리를 생성하여")
	fmt.Println("     불필요한 리소스 낭비와 초기화 실패 위험 방지")
	fmt.Println("     (ADR-003: docs/adr/003-lazy-storage-factory-initialization.md)")
	fmt.Println()
	fmt.Println("  3. Config 기반 전환: StorageConfig의 BackendType 값만 변경하면")
	fmt.Println("     런타임에 다른 스토리지 백엔드로 전환 가능")
	fmt.Println()
	fmt.Println("  4. 단일 Store, 이중 역할: MemoryStore와 FileStore 모두")
	fmt.Println("     Reader와 Writer 인터페이스를 동시에 구현하여")
	fmt.Println("     Factory가 동일한 인스턴스를 양쪽에 반환")
}
