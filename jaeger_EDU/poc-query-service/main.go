// Jaeger PoC: Query Service HTTP API 시뮬레이션
//
// Jaeger의 Query Service는 저장된 트레이스 데이터를 조회하기 위한
// HTTP API를 제공한다. Jaeger UI는 이 API를 통해 트레이스를 검색하고
// 시각화한다.
//
// 실제 Jaeger 소스 참조:
//   - cmd/jaeger/internal/extension/jaegerquery/internal/http_handler.go:
//     APIHandler, RegisterRoutes(), getServices(), search(), getTrace()
//   - structuredResponse: Data, Total, Limit, Offset, Errors 필드
//
// 주요 엔드포인트:
//   GET /api/services       → 서비스 목록 반환
//   GET /api/traces         → 조건부 트레이스 검색
//   GET /api/traces/{id}    → 특정 트레이스 조회
//   GET /api/operations     → 서비스별 오퍼레이션 목록

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================
// 데이터 모델 (Jaeger UI 모델 기반)
// ============================================================

// TraceID는 128비트 트레이스 식별자의 16진수 문자열 표현
type TraceID string

// SpanID는 64비트 span 식별자의 16진수 문자열 표현
type SpanID string

// KeyValue는 태그 키-값 쌍 (Jaeger UI 모델)
type KeyValue struct {
	Key   string      `json:"key"`
	Type  string      `json:"type"`
	Value interface{} `json:"value"`
}

// SpanLog는 span에 연결된 로그 이벤트
type SpanLog struct {
	Timestamp int64      `json:"timestamp"`
	Fields    []KeyValue `json:"fields"`
}

// SpanReference는 span 간의 관계 (CHILD_OF, FOLLOWS_FROM)
type SpanReference struct {
	RefType string  `json:"refType"`
	TraceID TraceID `json:"traceID"`
	SpanID  SpanID  `json:"spanID"`
}

// Process는 span을 생성한 프로세스 정보
type Process struct {
	ServiceName string     `json:"serviceName"`
	Tags        []KeyValue `json:"tags"`
}

// UISpan은 Jaeger UI에서 사용하는 span 포맷
type UISpan struct {
	TraceID       TraceID         `json:"traceID"`
	SpanID        SpanID          `json:"spanID"`
	OperationName string          `json:"operationName"`
	References    []SpanReference `json:"references"`
	StartTime     int64           `json:"startTime"` // 마이크로초
	Duration      int64           `json:"duration"`  // 마이크로초
	Tags          []KeyValue      `json:"tags"`
	Logs          []SpanLog       `json:"logs"`
	ProcessID     string          `json:"processID"`
	Warnings      []string        `json:"warnings"`
}

// UITrace는 Jaeger UI에서 사용하는 트레이스 포맷
type UITrace struct {
	TraceID   TraceID            `json:"traceID"`
	Spans     []UISpan           `json:"spans"`
	Processes map[string]Process `json:"processes"`
	Warnings  []string           `json:"warnings"`
}

// ============================================================
// API 응답 구조 (실제 Jaeger http_handler.go 참조)
// ============================================================

// StructuredResponse는 Jaeger API의 표준 응답 포맷
// 실제 소스: http_handler.go structuredResponse
type StructuredResponse struct {
	Data   interface{}       `json:"data"`
	Total  int               `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
	Errors []StructuredError `json:"errors"`
}

// StructuredError는 구조화된 에러 응답
type StructuredError struct {
	Code    int     `json:"code,omitempty"`
	Msg     string  `json:"msg"`
	TraceID TraceID `json:"traceID,omitempty"`
}

// Operation은 서비스의 오퍼레이션 정보
type Operation struct {
	Name     string `json:"name"`
	SpanKind string `json:"spanKind"`
}

// ============================================================
// 인메모리 스토리지
// ============================================================

// InMemoryStorage는 트레이스를 메모리에 저장하는 스토리지
type InMemoryStorage struct {
	mu     sync.RWMutex
	traces map[TraceID]*UITrace
}

func NewInMemoryStorage() *InMemoryStorage {
	return &InMemoryStorage{
		traces: make(map[TraceID]*UITrace),
	}
}

// WriteTrace는 트레이스를 저장한다
func (s *InMemoryStorage) WriteTrace(trace *UITrace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.traces[trace.TraceID] = trace
}

// GetTrace는 traceID로 트레이스를 조회한다
func (s *InMemoryStorage) GetTrace(traceID TraceID) (*UITrace, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	trace, ok := s.traces[traceID]
	return trace, ok
}

// GetServices는 저장된 모든 서비스 이름을 반환한다
func (s *InMemoryStorage) GetServices() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	serviceSet := make(map[string]struct{})
	for _, trace := range s.traces {
		for _, proc := range trace.Processes {
			serviceSet[proc.ServiceName] = struct{}{}
		}
	}

	services := make([]string, 0, len(serviceSet))
	for svc := range serviceSet {
		services = append(services, svc)
	}
	return services
}

// GetOperations는 서비스의 오퍼레이션 목록을 반환한다
func (s *InMemoryStorage) GetOperations(serviceName string) []Operation {
	s.mu.RLock()
	defer s.mu.RUnlock()

	opSet := make(map[string]string) // opName → spanKind
	for _, trace := range s.traces {
		for _, span := range trace.Spans {
			proc, ok := trace.Processes[span.ProcessID]
			if !ok {
				continue
			}
			if proc.ServiceName == serviceName {
				spanKind := ""
				for _, tag := range span.Tags {
					if tag.Key == "span.kind" {
						spanKind = fmt.Sprintf("%v", tag.Value)
					}
				}
				opSet[span.OperationName] = spanKind
			}
		}
	}

	operations := make([]Operation, 0, len(opSet))
	for name, kind := range opSet {
		operations = append(operations, Operation{Name: name, SpanKind: kind})
	}
	return operations
}

// FindTraces는 조건에 맞는 트레이스를 검색한다
func (s *InMemoryStorage) FindTraces(service string, operation string, startMin, startMax int64, limit int, tags map[string]string) []*UITrace {
	s.mu.RLock()
	defer s.mu.RUnlock()

	results := make([]*UITrace, 0)

	for _, trace := range s.traces {
		if matchesQuery(trace, service, operation, startMin, startMax, tags) {
			results = append(results, trace)
		}
		if limit > 0 && len(results) >= limit {
			break
		}
	}

	return results
}

func matchesQuery(trace *UITrace, service, operation string, startMin, startMax int64, tags map[string]string) bool {
	serviceMatch := service == ""
	operationMatch := operation == ""
	timeMatch := false

	for _, span := range trace.Spans {
		proc, ok := trace.Processes[span.ProcessID]
		if !ok {
			continue
		}

		if service != "" && proc.ServiceName == service {
			serviceMatch = true
		}
		if operation != "" && span.OperationName == operation {
			operationMatch = true
		}

		// 시간 범위 체크
		if (startMin == 0 || span.StartTime >= startMin) &&
			(startMax == 0 || span.StartTime <= startMax) {
			timeMatch = true
		}

		// 태그 매칭
		if len(tags) > 0 {
			for wantKey, wantVal := range tags {
				for _, tag := range span.Tags {
					if tag.Key == wantKey && fmt.Sprintf("%v", tag.Value) == wantVal {
						// 태그 일치
					}
				}
			}
		}
	}

	if startMin == 0 && startMax == 0 {
		timeMatch = true
	}

	return serviceMatch && operationMatch && timeMatch
}

// ============================================================
// API 핸들러 (실제 Jaeger http_handler.go 패턴 재현)
// ============================================================

// APIHandler는 Query Service HTTP 핸들러
// 실제 소스: http_handler.go APIHandler
type APIHandler struct {
	storage *InMemoryStorage
}

func NewAPIHandler(storage *InMemoryStorage) *APIHandler {
	return &APIHandler{storage: storage}
}

// RegisterRoutes는 HTTP 라우트를 등록한다
// 실제 소스: http_handler.go RegisterRoutes()
func (h *APIHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/services", h.getServices)
	mux.HandleFunc("GET /api/operations", h.getOperations)
	mux.HandleFunc("GET /api/traces/{traceID}", h.getTrace)
	mux.HandleFunc("GET /api/traces", h.findTraces)
}

// getServices는 서비스 목록을 반환한다
// 실제 소스: http_handler.go getServices()
func (h *APIHandler) getServices(w http.ResponseWriter, r *http.Request) {
	services := h.storage.GetServices()
	if len(services) == 0 {
		services = []string{}
	}

	resp := StructuredResponse{
		Data:  services,
		Total: len(services),
	}
	h.writeJSON(w, &resp)
}

// getOperations는 서비스의 오퍼레이션 목록을 반환한다
// 실제 소스: http_handler.go getOperations()
func (h *APIHandler) getOperations(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service == "" {
		h.handleError(w, "service parameter required", http.StatusBadRequest)
		return
	}

	operations := h.storage.GetOperations(service)
	resp := StructuredResponse{
		Data:  operations,
		Total: len(operations),
	}
	h.writeJSON(w, &resp)
}

// getTrace는 특정 traceID의 트레이스를 반환한다
// 실제 소스: http_handler.go getTrace()
func (h *APIHandler) getTrace(w http.ResponseWriter, r *http.Request) {
	traceIDStr := r.PathValue("traceID")
	if traceIDStr == "" {
		h.handleError(w, "traceID is required", http.StatusBadRequest)
		return
	}

	traceID := TraceID(traceIDStr)
	trace, ok := h.storage.GetTrace(traceID)
	if !ok {
		h.handleError(w, "trace not found", http.StatusNotFound)
		return
	}

	resp := StructuredResponse{
		Data:   []*UITrace{trace},
		Errors: []StructuredError{},
	}
	h.writeJSON(w, &resp)
}

// findTraces는 조건부 트레이스 검색을 수행한다
// 실제 소스: http_handler.go search()
func (h *APIHandler) findTraces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	service := q.Get("service")
	operation := q.Get("operation")
	limitStr := q.Get("limit")
	startStr := q.Get("start")
	endStr := q.Get("end")

	limit := 20
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			limit = l
		}
	}

	var startMin, startMax int64
	if startStr != "" {
		if t, err := strconv.ParseInt(startStr, 10, 64); err == nil {
			startMin = t
		}
	}
	if endStr != "" {
		if t, err := strconv.ParseInt(endStr, 10, 64); err == nil {
			startMax = t
		}
	}

	// 태그 파싱 (tag=key:value 형식)
	tags := make(map[string]string)
	for _, tagStr := range q["tag"] {
		parts := strings.SplitN(tagStr, ":", 2)
		if len(parts) == 2 {
			tags[parts[0]] = parts[1]
		}
	}

	traces := h.storage.FindTraces(service, operation, startMin, startMax, limit, tags)

	resp := StructuredResponse{
		Data:   traces,
		Total:  len(traces),
		Limit:  limit,
		Errors: []StructuredError{},
	}
	h.writeJSON(w, &resp)
}

// handleError는 에러 응답을 반환한다
// 실제 소스: http_handler.go handleError()
func (h *APIHandler) handleError(w http.ResponseWriter, msg string, statusCode int) {
	resp := StructuredResponse{
		Errors: []StructuredError{
			{
				Code: statusCode,
				Msg:  msg,
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(resp)
}

// writeJSON은 JSON 응답을 작성한다
// 실제 소스: http_handler.go writeJSON()
func (h *APIHandler) writeJSON(w http.ResponseWriter, response interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(response)
}

// ============================================================
// 샘플 데이터 생성
// ============================================================

func generateSampleTraces(storage *InMemoryStorage) {
	rng := rand.New(rand.NewSource(42))
	baseTime := time.Now().Add(-30 * time.Minute)

	serviceConfigs := []struct {
		name       string
		operations []string
		spanKind   string
	}{
		{"api-gateway", []string{"GET /users", "POST /orders", "GET /products", "GET /health"}, "server"},
		{"user-service", []string{"GetUser", "CreateUser", "UpdateUser"}, "server"},
		{"order-service", []string{"CreateOrder", "GetOrder", "ListOrders"}, "server"},
		{"payment-service", []string{"ProcessPayment", "RefundPayment"}, "server"},
		{"inventory-service", []string{"CheckStock", "ReserveItem"}, "server"},
		{"notification-service", []string{"SendEmail", "SendSMS"}, "server"},
		{"cache-service", []string{"Get", "Set", "Delete"}, "client"},
	}

	// 50개의 트레이스 생성 (각 트레이스는 3~7개의 span)
	for i := 0; i < 50; i++ {
		traceID := TraceID(fmt.Sprintf("%032x", i+1))
		traceStartTime := baseTime.Add(time.Duration(rng.Intn(1800)) * time.Second)

		numSpans := rng.Intn(5) + 3
		spans := make([]UISpan, 0, numSpans)
		processes := make(map[string]Process)

		// 루트 span (항상 api-gateway)
		rootSpanID := SpanID(fmt.Sprintf("%016x", rng.Int63()))
		rootOp := serviceConfigs[0].operations[rng.Intn(len(serviceConfigs[0].operations))]
		rootDuration := int64((rng.Intn(4000) + 500) * 1000) // 500ms ~ 4500ms in microseconds

		processes["p1"] = Process{
			ServiceName: "api-gateway",
			Tags: []KeyValue{
				{Key: "hostname", Type: "string", Value: "gateway-01"},
				{Key: "ip", Type: "string", Value: "10.0.1.1"},
			},
		}

		statusCodes := []string{"200", "201", "400", "404", "500"}
		statusWeights := []int{60, 10, 10, 10, 10}
		statusCode := weightedChoice(statusCodes, statusWeights, rng)

		spans = append(spans, UISpan{
			TraceID:       traceID,
			SpanID:        rootSpanID,
			OperationName: rootOp,
			References:    []SpanReference{},
			StartTime:     traceStartTime.UnixMicro(),
			Duration:      rootDuration,
			Tags: []KeyValue{
				{Key: "http.method", Type: "string", Value: "GET"},
				{Key: "http.status_code", Type: "int64", Value: statusCode},
				{Key: "span.kind", Type: "string", Value: "server"},
			},
			Logs:      []SpanLog{},
			ProcessID: "p1",
			Warnings:  []string{},
		})

		// 자식 span 생성
		parentSpanID := rootSpanID
		childStartTime := traceStartTime.Add(time.Duration(rng.Intn(50)) * time.Millisecond)

		for j := 1; j < numSpans; j++ {
			svcIdx := rng.Intn(len(serviceConfigs)-1) + 1
			svc := serviceConfigs[svcIdx]
			op := svc.operations[rng.Intn(len(svc.operations))]
			spanID := SpanID(fmt.Sprintf("%016x", rng.Int63()))
			duration := int64((rng.Intn(2000) + 100) * 1000) // 100ms ~ 2100ms

			processID := fmt.Sprintf("p%d", j+1)
			processes[processID] = Process{
				ServiceName: svc.name,
				Tags: []KeyValue{
					{Key: "hostname", Type: "string", Value: fmt.Sprintf("%s-%02d", svc.name, rng.Intn(5)+1)},
				},
			}

			childTags := []KeyValue{
				{Key: "span.kind", Type: "string", Value: svc.spanKind},
			}

			// 일부 span에 에러 태그 추가
			if rng.Float64() < 0.1 {
				childTags = append(childTags, KeyValue{Key: "error", Type: "bool", Value: true})
			}

			// 일부 span에 DB 관련 태그 추가
			if rng.Float64() < 0.3 {
				childTags = append(childTags, KeyValue{Key: "db.type", Type: "string", Value: "postgresql"})
				childTags = append(childTags, KeyValue{Key: "db.statement", Type: "string", Value: "SELECT * FROM ..."})
			}

			spans = append(spans, UISpan{
				TraceID:       traceID,
				SpanID:        spanID,
				OperationName: op,
				References: []SpanReference{
					{RefType: "CHILD_OF", TraceID: traceID, SpanID: parentSpanID},
				},
				StartTime: childStartTime.UnixMicro(),
				Duration:  duration,
				Tags:      childTags,
				Logs:      []SpanLog{},
				ProcessID: processID,
				Warnings:  []string{},
			})

			childStartTime = childStartTime.Add(time.Duration(rng.Intn(100)) * time.Millisecond)
			// 50% 확률로 자식의 자식으로 연결
			if rng.Float64() < 0.5 {
				parentSpanID = spanID
			}
		}

		trace := &UITrace{
			TraceID:   traceID,
			Spans:     spans,
			Processes: processes,
			Warnings:  []string{},
		}
		storage.WriteTrace(trace)
	}
}

func weightedChoice(choices []string, weights []int, rng *rand.Rand) string {
	total := 0
	for _, w := range weights {
		total += w
	}
	r := rng.Intn(total)
	cumulative := 0
	for i, w := range weights {
		cumulative += w
		if r < cumulative {
			return choices[i]
		}
	}
	return choices[len(choices)-1]
}

// ============================================================
// HTTP 클라이언트 시뮬레이션 (서버 테스트용)
// ============================================================

func simulateClientRequests(baseURL string) {
	fmt.Println("\n  HTTP 클라이언트로 API 요청 시뮬레이션:")
	fmt.Println(strings.Repeat("-", 60))

	client := &http.Client{Timeout: 5 * time.Second}

	// 1. GET /api/services
	fmt.Println("\n  [요청 1] GET /api/services")
	resp, err := client.Get(baseURL + "/api/services")
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	var servicesResp StructuredResponse
	json.NewDecoder(resp.Body).Decode(&servicesResp)
	resp.Body.Close()
	fmt.Printf("    상태 코드: %d\n", resp.StatusCode)
	prettyJSON, _ := json.MarshalIndent(servicesResp, "    ", "  ")
	fmt.Printf("    응답:\n    %s\n", string(prettyJSON))

	// 2. GET /api/operations?service=api-gateway
	fmt.Println("\n  [요청 2] GET /api/operations?service=api-gateway")
	resp, err = client.Get(baseURL + "/api/operations?service=api-gateway")
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	var opsResp StructuredResponse
	json.NewDecoder(resp.Body).Decode(&opsResp)
	resp.Body.Close()
	fmt.Printf("    상태 코드: %d\n", resp.StatusCode)
	prettyJSON, _ = json.MarshalIndent(opsResp, "    ", "  ")
	fmt.Printf("    응답:\n    %s\n", string(prettyJSON))

	// 3. GET /api/traces?service=api-gateway&limit=3
	fmt.Println("\n  [요청 3] GET /api/traces?service=api-gateway&limit=3")
	resp, err = client.Get(baseURL + "/api/traces?service=api-gateway&limit=3")
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	var tracesResp StructuredResponse
	json.NewDecoder(resp.Body).Decode(&tracesResp)
	resp.Body.Close()
	fmt.Printf("    상태 코드: %d\n", resp.StatusCode)
	fmt.Printf("    총 트레이스 수: %d\n", tracesResp.Total)

	// data를 다시 JSON 인코딩하여 일부만 출력
	if data, ok := tracesResp.Data.([]interface{}); ok && len(data) > 0 {
		firstTrace, _ := json.MarshalIndent(data[0], "    ", "  ")
		// 너무 길면 잘라서 출력
		s := string(firstTrace)
		if len(s) > 500 {
			s = s[:500] + "\n    ... (이하 생략)"
		}
		fmt.Printf("    첫 번째 트레이스 (일부):\n    %s\n", s)
	}

	// 4. GET /api/traces/{traceID}
	traceID := fmt.Sprintf("%032x", 1) // 첫 번째 트레이스
	fmt.Printf("\n  [요청 4] GET /api/traces/%s\n", traceID)
	resp, err = client.Get(baseURL + "/api/traces/" + traceID)
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	var traceResp StructuredResponse
	json.NewDecoder(resp.Body).Decode(&traceResp)
	resp.Body.Close()
	fmt.Printf("    상태 코드: %d\n", resp.StatusCode)

	if data, ok := traceResp.Data.([]interface{}); ok && len(data) > 0 {
		if traceMap, ok := data[0].(map[string]interface{}); ok {
			if spans, ok := traceMap["spans"].([]interface{}); ok {
				fmt.Printf("    트레이스 ID: %s\n", traceID)
				fmt.Printf("    span 수: %d\n", len(spans))
			}
			if procs, ok := traceMap["processes"].(map[string]interface{}); ok {
				fmt.Printf("    프로세스 수: %d\n", len(procs))
				for pid, proc := range procs {
					if procMap, ok := proc.(map[string]interface{}); ok {
						fmt.Printf("      %s: %s\n", pid, procMap["serviceName"])
					}
				}
			}
		}
	}

	// 5. GET /api/traces/{존재하지 않는 ID}
	notFoundID := "ffffffffffffffffffffffffffffffff"
	fmt.Printf("\n  [요청 5] GET /api/traces/%s (존재하지 않는 ID)\n", notFoundID)
	resp, err = client.Get(baseURL + "/api/traces/" + notFoundID)
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	var notFoundResp StructuredResponse
	json.NewDecoder(resp.Body).Decode(&notFoundResp)
	resp.Body.Close()
	fmt.Printf("    상태 코드: %d\n", resp.StatusCode)
	prettyJSON, _ = json.MarshalIndent(notFoundResp, "    ", "  ")
	fmt.Printf("    응답:\n    %s\n", string(prettyJSON))
}

// ============================================================
// 메인
// ============================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("  Jaeger PoC: Query Service HTTP API 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	// ----------------------------------------------------------
	// 1. 스토리지 초기화 및 샘플 데이터 생성
	// ----------------------------------------------------------
	fmt.Println("\n[1단계] 인메모리 스토리지 초기화 및 샘플 데이터 생성")
	fmt.Println(strings.Repeat("-", 60))

	storage := NewInMemoryStorage()
	generateSampleTraces(storage)

	services := storage.GetServices()
	fmt.Printf("  등록된 서비스 수: %d\n", len(services))
	for _, svc := range services {
		ops := storage.GetOperations(svc)
		fmt.Printf("    - %s (%d개 오퍼레이션)\n", svc, len(ops))
	}
	fmt.Printf("  저장된 트레이스 수: %d\n", len(storage.traces))

	// ----------------------------------------------------------
	// 2. API 라우트 구조 설명
	// ----------------------------------------------------------
	fmt.Println("\n[2단계] API 라우트 구조")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  실제 Jaeger http_handler.go RegisterRoutes() 기반:

  ┌─────────────────────────────────────────────────────────────┐
  │  HTTP API 엔드포인트                                         │
  ├─────────────────────────────────────────────────────────────┤
  │  GET  /api/services                → getServices()          │
  │  GET  /api/operations?service=X    → getOperations()        │
  │  GET  /api/traces?service=X&...    → search() / findTraces()│
  │  GET  /api/traces/{traceID}        → getTrace()             │
  │  GET  /api/dependencies            → dependencies()         │
  │  POST /api/archive/{traceID}       → archiveTrace()         │
  └─────────────────────────────────────────────────────────────┘

  응답 구조 (structuredResponse):
  {
    "data":   [...],      // 실제 데이터
    "total":  N,          // 전체 결과 수
    "limit":  M,          // 요청된 제한
    "offset": 0,          // 페이징 오프셋
    "errors": [...]       // 에러 목록
  }`)

	// ----------------------------------------------------------
	// 3. 쿼리 플로우 시각화
	// ----------------------------------------------------------
	fmt.Println("\n[3단계] 쿼리 서비스 내부 흐름")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  HTTP 요청 → 쿼리 서비스 → 스토리지 흐름:

  ┌──────────┐    ┌───────────────────┐    ┌──────────────┐
  │ Jaeger   │───>│  APIHandler       │───>│  Storage     │
  │ UI/CLI   │    │  (http_handler.go)│    │  Backend     │
  └──────────┘    └───────┬───────────┘    └──────┬───────┘
                          │                        │
  1. HTTP Request ────────┤                        │
  2. Parse params ────────┤                        │
  3. queryParser.parse ───┤                        │
  4. queryService.Find ───┼────────────────────────┤
  5. storage.FindTraces ──┼────────────────────────┤
  6. Format response ─────┤                        │
  7. JSON response ───────┤                        │
                          │                        │

  * 실제 Jaeger는 QueryService를 통해 스토리지에 접근
  * 스토리지 백엔드: Badger, Cassandra, Elasticsearch, gRPC-plugin 등
  * UI 모델로 변환: uiconv.FromDomain()으로 내부 모델 → UI JSON 변환`)

	// ----------------------------------------------------------
	// 4. HTTP 서버 시작 및 요청 시뮬레이션
	// ----------------------------------------------------------
	fmt.Println("\n[4단계] HTTP 서버 시작 및 API 요청 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))

	// 사용 가능한 포트 찾기
	port := 16686 // Jaeger 기본 UI 포트
	baseURL := fmt.Sprintf("http://localhost:%d", port)

	mux := http.NewServeMux()
	handler := NewAPIHandler(storage)
	handler.RegisterRoutes(mux)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	fmt.Printf("  서버 시작: %s\n", baseURL)
	fmt.Println("  (Jaeger 기본 UI 포트: 16686)")

	// 서버를 고루틴으로 시작
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("  서버 에러: %v\n", err)
		}
	}()

	// 서버가 준비될 때까지 잠시 대기
	time.Sleep(100 * time.Millisecond)

	// 클라이언트 요청 시뮬레이션
	simulateClientRequests(baseURL)

	// ----------------------------------------------------------
	// 5. 서비스 아키텍처 요약
	// ----------------------------------------------------------
	fmt.Println("\n[5단계] Query Service 아키텍처 요약")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  실제 Jaeger Query Service 구조:

  ┌─────────────────────────────────────────────────────────┐
  │                    Jaeger Query                          │
  ├─────────────────────────────────────────────────────────┤
  │                                                          │
  │  ┌──────────────┐    ┌──────────────┐                   │
  │  │ HTTP Handler │    │ gRPC Handler │                   │
  │  │ (APIHandler) │    │ (GRPCHandler)│                   │
  │  └──────┬───────┘    └──────┬───────┘                   │
  │         │                    │                           │
  │         └────────┬───────────┘                           │
  │                  │                                       │
  │         ┌────────▼────────┐                              │
  │         │  QueryService   │  ← 비즈니스 로직             │
  │         │  (querysvc)     │                              │
  │         └────────┬────────┘                              │
  │                  │                                       │
  │         ┌────────▼────────┐                              │
  │         │ Storage Backend │  ← 스토리지 추상화           │
  │         │  (spanstore)    │                              │
  │         └────────┬────────┘                              │
  │                  │                                       │
  │    ┌─────────────┼─────────────┐                         │
  │    │             │             │                          │
  │  ┌─▼──┐    ┌────▼──┐   ┌────▼────┐                     │
  │  │Badger│  │Cassandra│  │Elastic  │                     │
  │  └─────┘    └───────┘   └─────────┘                     │
  │                                                          │
  └─────────────────────────────────────────────────────────┘

  핵심 포인트:
  1. HTTP/gRPC 이중 프로토콜 지원
  2. QueryService가 스토리지 추상화 계층 역할
  3. structuredResponse 통일 응답 포맷
  4. uiconv.FromDomain()으로 내부 모델 → UI JSON 변환`)

	// 서버 종료
	server.Close()

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("  시뮬레이션 완료 (서버 종료)")
	fmt.Println(strings.Repeat("=", 80))
}
