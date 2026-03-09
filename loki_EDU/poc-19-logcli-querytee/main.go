// Package main은 Loki LogCLI와 Query Tee의 핵심 개념을 시뮬레이션한다.
//
// 시뮬레이션하는 핵심 개념:
// 1. LogCLI 스타일 커맨드 디스패치 (서브커맨드 + 플래그 파싱)
// 2. HTTP API 클라이언트 추상화 (DefaultClient vs FileClient)
// 3. 병렬 쿼리 실행 (시간 범위 분할 + 워커 풀)
// 4. Query Tee 프록시 (듀얼 백엔드 + 응답 비교)
// 5. 출력 포매터 시스템 (default, raw, jsonl)
//
// 실행: go run main.go
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────
// 1. LogCLI 커맨드 디스패치 시스템
// ─────────────────────────────────────────────

// Command는 LogCLI 서브커맨드를 나타낸다.
type Command struct {
	Name        string
	Description string
	Execute     func(args map[string]string) error
}

// CLI는 LogCLI 스타일의 명령줄 인터페이스를 시뮬레이션한다.
type CLI struct {
	name     string
	commands map[string]*Command
	globals  map[string]string
}

func NewCLI(name string) *CLI {
	return &CLI{
		name:     name,
		commands: make(map[string]*Command),
		globals:  make(map[string]string),
	}
}

func (c *CLI) RegisterCommand(cmd *Command) {
	c.commands[cmd.Name] = cmd
}

func (c *CLI) SetGlobal(key, value string) {
	c.globals[key] = value
}

func (c *CLI) Execute(cmdName string, args map[string]string) error {
	cmd, ok := c.commands[cmdName]
	if !ok {
		return fmt.Errorf("unknown command: %s", cmdName)
	}
	// 글로벌 플래그를 args에 병합
	merged := make(map[string]string)
	for k, v := range c.globals {
		merged[k] = v
	}
	for k, v := range args {
		merged[k] = v
	}
	return cmd.Execute(merged)
}

// ─────────────────────────────────────────────
// 2. Loki API 클라이언트 추상화
// ─────────────────────────────────────────────

// LogEntry는 로그 엔트리를 나타낸다.
type LogEntry struct {
	Timestamp time.Time
	Labels    map[string]string
	Line      string
}

// QueryResult는 쿼리 결과를 나타낸다.
type QueryResult struct {
	Status  string     `json:"status"`
	Entries []LogEntry `json:"entries"`
}

// Client는 LogCLI 클라이언트 인터페이스이다.
type Client interface {
	QueryRange(query string, start, end time.Time, limit int) (*QueryResult, error)
	Labels() ([]string, error)
	GetOrgID() string
}

// HTTPClient는 HTTP API를 통한 Loki 클라이언트이다.
type HTTPClient struct {
	Address  string
	OrgID    string
	Username string
	Password string
}

func (c *HTTPClient) QueryRange(query string, start, end time.Time, limit int) (*QueryResult, error) {
	// 실제 Loki HTTP API 호출을 시뮬레이션
	entries := generateFakeLogs(start, end, limit, query)
	return &QueryResult{Status: "success", Entries: entries}, nil
}

func (c *HTTPClient) Labels() ([]string, error) {
	return []string{"app", "env", "level", "pod"}, nil
}

func (c *HTTPClient) GetOrgID() string {
	return c.OrgID
}

// FileClient는 stdin/파일 기반 클라이언트이다 (stdin 모드).
type FileClient struct {
	lines []string
}

func NewFileClient(lines []string) *FileClient {
	return &FileClient{lines: lines}
}

func (c *FileClient) QueryRange(query string, start, end time.Time, limit int) (*QueryResult, error) {
	// 내장된 라인에서 검색
	entries := make([]LogEntry, 0)
	filter := extractFilter(query)
	for _, line := range c.lines {
		if filter == "" || strings.Contains(line, filter) {
			entries = append(entries, LogEntry{
				Timestamp: time.Now(),
				Labels:    map[string]string{"source": "file"},
				Line:      line,
			})
		}
	}
	return &QueryResult{Status: "success", Entries: entries}, nil
}

func (c *FileClient) Labels() ([]string, error) {
	return []string{"source"}, nil
}

func (c *FileClient) GetOrgID() string {
	return ""
}

// extractFilter는 LogQL에서 필터 부분을 추출한다.
func extractFilter(query string) string {
	if idx := strings.Index(query, `|="`); idx >= 0 {
		end := strings.Index(query[idx+3:], `"`)
		if end >= 0 {
			return query[idx+3 : idx+3+end]
		}
	}
	return ""
}

// ─────────────────────────────────────────────
// 3. 병렬 쿼리 실행 시스템
// ─────────────────────────────────────────────

// ParallelQuery는 시간 범위 분할 기반 병렬 쿼리를 수행한다.
type ParallelQuery struct {
	Client           Client
	Query            string
	Start            time.Time
	End              time.Time
	ParallelDuration time.Duration
	MaxWorkers       int
}

// SyncRange는 하나의 병렬 작업 범위이다.
type SyncRange struct {
	Number int
	From   time.Time
	To     time.Time
}

func (pq *ParallelQuery) CalcSyncRanges() []SyncRange {
	var ranges []SyncRange
	current := pq.Start
	number := 0
	for current.Before(pq.End) {
		rangeEnd := current.Add(pq.ParallelDuration)
		if rangeEnd.After(pq.End) {
			rangeEnd = pq.End
		}
		ranges = append(ranges, SyncRange{
			Number: number,
			From:   current,
			To:     rangeEnd,
		})
		current = rangeEnd
		number++
	}
	return ranges
}

func (pq *ParallelQuery) Execute() (*QueryResult, error) {
	ranges := pq.CalcSyncRanges()
	fmt.Printf("  병렬 쿼리: %d개 범위, %d개 워커\n", len(ranges), pq.MaxWorkers)

	rangeCh := make(chan SyncRange, len(ranges))
	resultCh := make(chan []LogEntry, len(ranges))
	var wg sync.WaitGroup

	// 워커 시작
	for i := 0; i < pq.MaxWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for sr := range rangeCh {
				result, err := pq.Client.QueryRange(pq.Query, sr.From, sr.To, 0)
				if err != nil {
					fmt.Printf("  워커 %d: 범위 %d 오류: %v\n", workerID, sr.Number, err)
					continue
				}
				fmt.Printf("  워커 %d: 범위 %d 완료 (%d 엔트리)\n",
					workerID, sr.Number, len(result.Entries))
				resultCh <- result.Entries
			}
		}(i)
	}

	// 작업 분배
	for _, sr := range ranges {
		rangeCh <- sr
	}
	close(rangeCh)

	// 완료 대기
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// 결과 수집
	allEntries := make([]LogEntry, 0)
	for entries := range resultCh {
		allEntries = append(allEntries, entries...)
	}

	return &QueryResult{Status: "success", Entries: allEntries}, nil
}

// defaultQueryRangeStep은 Loki 서버와 동일한 step 계산 로직이다.
func defaultQueryRangeStep(start, end time.Time) time.Duration {
	step := int(math.Max(math.Floor(end.Sub(start).Seconds()/250), 1))
	return time.Duration(step) * time.Second
}

// ─────────────────────────────────────────────
// 4. Query Tee 프록시 시스템
// ─────────────────────────────────────────────

// Route는 Query Tee의 프록시 라우트를 정의한다.
type Route struct {
	Path       string
	RouteName  string
	Methods    []string
	Comparator ResponseComparator
}

// ResponseComparator는 두 응답을 비교한다.
type ResponseComparator interface {
	Compare(expected, actual []byte) (bool, string)
}

// SamplesComparator는 메트릭 샘플 값을 비교한다.
type SamplesComparator struct {
	Tolerance float64
}

func (sc *SamplesComparator) Compare(expected, actual []byte) (bool, string) {
	var expResult, actResult map[string]interface{}
	if err := json.Unmarshal(expected, &expResult); err != nil {
		return false, fmt.Sprintf("expected parse error: %v", err)
	}
	if err := json.Unmarshal(actual, &actResult); err != nil {
		return false, fmt.Sprintf("actual parse error: %v", err)
	}

	expStatus, _ := expResult["status"].(string)
	actStatus, _ := actResult["status"].(string)
	if expStatus != actStatus {
		return false, fmt.Sprintf("status mismatch: %s vs %s", expStatus, actStatus)
	}
	return true, "match"
}

// QueryTeeProxy는 두 백엔드에 쿼리를 전달하고 응답을 비교한다.
type QueryTeeProxy struct {
	PreferredBackend string
	SecondaryBackend string
	Routes           []Route
	mu               sync.Mutex
	stats            ProxyStats
}

type ProxyStats struct {
	TotalRequests int
	Matches       int
	Mismatches    int
}

func NewQueryTeeProxy(preferred, secondary string, routes []Route) *QueryTeeProxy {
	return &QueryTeeProxy{
		PreferredBackend: preferred,
		SecondaryBackend: secondary,
		Routes:           routes,
	}
}

func (p *QueryTeeProxy) HandleRequest(path, method string) ([]byte, error) {
	// 라우트 매칭
	var route *Route
	for i, r := range p.Routes {
		if r.Path == path {
			for _, m := range r.Methods {
				if m == method {
					route = &p.Routes[i]
					break
				}
			}
		}
	}
	if route == nil {
		return nil, fmt.Errorf("no route for %s %s", method, path)
	}

	// 양쪽 백엔드에 쿼리 전달 (병렬)
	type backendResult struct {
		name string
		data []byte
		err  error
	}

	results := make(chan backendResult, 2)
	go func() {
		data := []byte(fmt.Sprintf(`{"status":"success","backend":"%s","values":[1.0,2.0,3.0]}`, p.PreferredBackend))
		results <- backendResult{name: "preferred", data: data}
	}()
	go func() {
		data := []byte(fmt.Sprintf(`{"status":"success","backend":"%s","values":[1.0,2.0,3.0]}`, p.SecondaryBackend))
		results <- backendResult{name: "secondary", data: data}
	}()

	var preferred, secondary backendResult
	for i := 0; i < 2; i++ {
		r := <-results
		if r.name == "preferred" {
			preferred = r
		} else {
			secondary = r
		}
	}

	p.mu.Lock()
	p.stats.TotalRequests++

	// 응답 비교 (비교기가 있는 경우)
	if route.Comparator != nil {
		match, reason := route.Comparator.Compare(preferred.data, secondary.data)
		if match {
			p.stats.Matches++
		} else {
			p.stats.Mismatches++
			fmt.Printf("  [Query Tee] 불일치 감지: %s (%s)\n", route.RouteName, reason)
		}
	}
	p.mu.Unlock()

	// 항상 preferred 백엔드의 응답 반환
	return preferred.data, preferred.err
}

// ─────────────────────────────────────────────
// 5. 출력 포매터 시스템
// ─────────────────────────────────────────────

// OutputFormatter는 로그 출력 형식을 정의한다.
type OutputFormatter interface {
	Format(entry LogEntry) string
}

type DefaultFormatter struct {
	TimestampFormat string
	NoLabels        bool
}

func (f *DefaultFormatter) Format(entry LogEntry) string {
	ts := entry.Timestamp.Format(f.TimestampFormat)
	if f.NoLabels {
		return fmt.Sprintf("%s %s", ts, entry.Line)
	}
	labels := formatLabels(entry.Labels)
	return fmt.Sprintf("%s %s %s", ts, labels, entry.Line)
}

type RawFormatter struct{}

func (f *RawFormatter) Format(entry LogEntry) string {
	return entry.Line
}

type JSONLFormatter struct{}

func (f *JSONLFormatter) Format(entry LogEntry) string {
	data, _ := json.Marshal(entry)
	return string(data)
}

func formatLabels(labels map[string]string) string {
	parts := make([]string, 0)
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%q", k, v))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// ─────────────────────────────────────────────
// 헬퍼 함수
// ─────────────────────────────────────────────

func generateFakeLogs(start, end time.Time, limit int, query string) []LogEntry {
	if limit <= 0 {
		limit = 5
	}
	entries := make([]LogEntry, 0, limit)
	duration := end.Sub(start)
	step := duration / time.Duration(limit)

	levels := []string{"INFO", "WARN", "ERROR", "DEBUG"}
	messages := []string{
		"GET /api/v1/users 200 12ms",
		"POST /api/v1/orders 201 45ms",
		"GET /api/v1/health 200 1ms",
		"DELETE /api/v1/cache 204 3ms",
		"GET /api/v1/metrics 200 8ms",
	}

	for i := 0; i < limit; i++ {
		ts := start.Add(step * time.Duration(i))
		entries = append(entries, LogEntry{
			Timestamp: ts,
			Labels: map[string]string{
				"app":   "web-server",
				"env":   "production",
				"level": levels[rand.Intn(len(levels))],
			},
			Line: fmt.Sprintf("[%s] %s", levels[rand.Intn(len(levels))], messages[rand.Intn(len(messages))]),
		})
	}
	return entries
}

// ─────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║  Loki LogCLI & Query Tee 시뮬레이션              ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. LogCLI 커맨드 시스템 데모 ===
	fmt.Println("━━━ 1. LogCLI 커맨드 디스패치 ━━━")
	cli := NewCLI("logcli")
	cli.SetGlobal("addr", "http://localhost:3100")
	cli.SetGlobal("output", "default")

	httpClient := &HTTPClient{
		Address: "http://localhost:3100",
		OrgID:   "tenant-1",
	}

	cli.RegisterCommand(&Command{
		Name: "query",
		Execute: func(args map[string]string) error {
			fmt.Printf("  커맨드: query\n")
			fmt.Printf("  서버: %s, 테넌트: %s\n", args["addr"], httpClient.OrgID)
			end := time.Now()
			start := end.Add(-1 * time.Hour)
			result, err := httpClient.QueryRange(`{app="web-server"}`, start, end, 5)
			if err != nil {
				return err
			}
			fmt.Printf("  결과: %d개 로그\n", len(result.Entries))
			return nil
		},
	})

	cli.RegisterCommand(&Command{
		Name: "labels",
		Execute: func(args map[string]string) error {
			labels, err := httpClient.Labels()
			if err != nil {
				return err
			}
			fmt.Printf("  레이블: %v\n", labels)
			return nil
		},
	})

	cli.Execute("query", map[string]string{"query": `{app="web"}`})
	cli.Execute("labels", nil)
	fmt.Println()

	// === 2. FileClient (stdin 모드) 데모 ===
	fmt.Println("━━━ 2. FileClient (stdin 모드) ━━━")
	fileLines := []string{
		"2024-01-15T10:00:01Z [INFO] GET /api/v1/users 200",
		"2024-01-15T10:00:02Z [ERROR] POST /api/v1/orders 500",
		"2024-01-15T10:00:03Z [INFO] GET /api/v1/health 200",
		"2024-01-15T10:00:04Z [WARN] GET /api/v1/users 200 slow",
		"2024-01-15T10:00:05Z [ERROR] database connection timeout",
	}
	fc := NewFileClient(fileLines)
	result, _ := fc.QueryRange(`{source="file"} |="ERROR"`, time.Now().Add(-1*time.Hour), time.Now(), 0)
	fmt.Printf("  stdin에서 ERROR 필터: %d개 매칭\n", len(result.Entries))
	for _, e := range result.Entries {
		fmt.Printf("    %s\n", e.Line)
	}
	fmt.Println()

	// === 3. 출력 포매터 데모 ===
	fmt.Println("━━━ 3. 출력 포매터 비교 ━━━")
	sampleEntry := LogEntry{
		Timestamp: time.Date(2024, 1, 15, 10, 0, 1, 0, time.UTC),
		Labels:    map[string]string{"app": "web", "level": "INFO"},
		Line:      "GET /api/v1/users 200 12ms",
	}

	formatters := map[string]OutputFormatter{
		"default": &DefaultFormatter{TimestampFormat: time.RFC3339, NoLabels: false},
		"raw":     &RawFormatter{},
		"jsonl":   &JSONLFormatter{},
	}

	for name, f := range formatters {
		fmt.Printf("  [%s] %s\n", name, f.Format(sampleEntry))
	}
	fmt.Println()

	// === 4. 병렬 쿼리 데모 ===
	fmt.Println("━━━ 4. 병렬 쿼리 실행 ━━━")
	end := time.Now()
	start := end.Add(-10 * time.Hour)
	pq := &ParallelQuery{
		Client:           httpClient,
		Query:            `{app="web-server"}`,
		Start:            start,
		End:              end,
		ParallelDuration: 2 * time.Hour,
		MaxWorkers:       3,
	}

	ranges := pq.CalcSyncRanges()
	fmt.Printf("  시간 범위: %s ~ %s\n", start.Format(time.RFC3339), end.Format(time.RFC3339))
	fmt.Printf("  분할 단위: 2h → %d개 범위\n", len(ranges))

	// step 자동 계산
	step := defaultQueryRangeStep(start, end)
	fmt.Printf("  자동 step 계산: %v (범위/250)\n", step)

	parallelResult, _ := pq.Execute()
	fmt.Printf("  병렬 쿼리 결과: %d개 총 엔트리\n", len(parallelResult.Entries))
	fmt.Println()

	// === 5. Query Tee 프록시 데모 ===
	fmt.Println("━━━ 5. Query Tee 프록시 ━━━")

	// 두 개의 테스트 서버 생성
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "backend": "A"})
	}))
	defer serverA.Close()

	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "backend": "B"})
	}))
	defer serverB.Close()

	routes := []Route{
		{Path: "/loki/api/v1/query_range", RouteName: "query_range",
			Methods: []string{"GET", "POST"}, Comparator: &SamplesComparator{Tolerance: 0.001}},
		{Path: "/loki/api/v1/query", RouteName: "instant_query",
			Methods: []string{"GET", "POST"}, Comparator: &SamplesComparator{Tolerance: 0.001}},
		{Path: "/loki/api/v1/labels", RouteName: "labels",
			Methods: []string{"GET"}, Comparator: nil},
		{Path: "/loki/api/v1/push", RouteName: "push",
			Methods: []string{"POST"}, Comparator: nil},
	}

	proxy := NewQueryTeeProxy(serverA.URL, serverB.URL, routes)

	fmt.Printf("  Backend A: %s\n", serverA.URL)
	fmt.Printf("  Backend B: %s\n", serverB.URL)
	fmt.Printf("  등록된 라우트: %d개\n", len(routes))

	// 여러 요청 시뮬레이션
	testRequests := []struct {
		path   string
		method string
	}{
		{"/loki/api/v1/query_range", "GET"},
		{"/loki/api/v1/query", "POST"},
		{"/loki/api/v1/labels", "GET"},
		{"/loki/api/v1/query_range", "GET"},
		{"/loki/api/v1/push", "POST"},
	}

	for _, req := range testRequests {
		resp, err := proxy.HandleRequest(req.path, req.method)
		if err != nil {
			fmt.Printf("  %s %s → 오류: %v\n", req.method, req.path, err)
		} else {
			fmt.Printf("  %s %s → %d bytes 응답\n", req.method, req.path, len(resp))
		}
	}

	fmt.Printf("\n  프록시 통계:\n")
	fmt.Printf("    총 요청: %d\n", proxy.stats.TotalRequests)
	fmt.Printf("    일치: %d\n", proxy.stats.Matches)
	fmt.Printf("    불일치: %d\n", proxy.stats.Mismatches)

	fmt.Println()
	fmt.Println("━━━ 라우트 매핑 테이블 ━━━")
	fmt.Printf("  %-35s %-10s %-10s\n", "경로", "메서드", "비교기")
	fmt.Println("  " + strings.Repeat("─", 55))
	for _, r := range routes {
		comp := "없음"
		if r.Comparator != nil {
			comp = "Samples"
		}
		fmt.Printf("  %-35s %-10s %-10s\n", r.Path, strings.Join(r.Methods, ","), comp)
	}

	fmt.Println()
	fmt.Println("시뮬레이션 완료.")
}
