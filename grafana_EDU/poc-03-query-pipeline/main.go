package main

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 1. 핵심 데이터 구조 — Grafana의 data package 모델
// ---------------------------------------------------------------------------

// FieldType — 필드 데이터 타입
type FieldType string

const (
	FieldTypeTime    FieldType = "time"
	FieldTypeFloat64 FieldType = "float64"
	FieldTypeString  FieldType = "string"
	FieldTypeInt64   FieldType = "int64"
)

// Field — DataFrame의 열 (Grafana의 data.Field에 해당)
type Field struct {
	Name   string
	Type   FieldType
	Values []interface{}
	Labels map[string]string // 시계열 라벨
}

// DataFrameMeta — DataFrame 메타 정보
type DataFrameMeta struct {
	ExecutedQueryString string
	Custom              map[string]interface{}
}

// DataFrame — 쿼리 결과 데이터 (Grafana의 data.Frame에 해당)
// 모든 데이터소스의 응답은 DataFrame으로 통일된다.
type DataFrame struct {
	Name   string
	Fields []*Field
	Meta   *DataFrameMeta
}

// Rows — DataFrame을 행 단위로 출력
func (df *DataFrame) Rows() int {
	if len(df.Fields) == 0 {
		return 0
	}
	return len(df.Fields[0].Values)
}

// ---------------------------------------------------------------------------
// 2. 쿼리 요청/응답 모델
// ---------------------------------------------------------------------------

// DataQuery — 단일 쿼리 (패널의 Target에 해당)
type DataQuery struct {
	RefID         string // "A", "B", ...
	DatasourceUID string
	QueryType     string
	Expr          string // 쿼리 표현식
	IntervalMs    int64
	MaxDataPoints int64
}

// TimeRange — 쿼리 시간 범위
type TimeRange struct {
	From time.Time
	To   time.Time
}

// QueryDataRequest — 쿼리 실행 요청
type QueryDataRequest struct {
	Queries   []DataQuery
	TimeRange TimeRange
	Headers   map[string]string
}

// DataResponse — 단일 쿼리의 응답
type DataResponse struct {
	Frames []*DataFrame
	Error  error
	Status int // HTTP status code
}

// QueryDataResponse — 전체 쿼리 응답 (RefID → DataResponse)
type QueryDataResponse struct {
	Responses map[string]DataResponse
}

// ---------------------------------------------------------------------------
// 3. DataSource 플러그인 인터페이스
// ---------------------------------------------------------------------------

// DataSourcePlugin — 데이터소스 플러그인이 구현해야 하는 인터페이스
type DataSourcePlugin interface {
	PluginID() string
	QueryData(ctx context.Context, req *QueryDataRequest) (*QueryDataResponse, error)
}

// ---------------------------------------------------------------------------
// 4. Prometheus 데이터소스 시뮬레이션
// ---------------------------------------------------------------------------

type prometheusDS struct {
	uid string
	url string
}

func (p *prometheusDS) PluginID() string { return "prometheus" }

func (p *prometheusDS) QueryData(ctx context.Context, req *QueryDataRequest) (*QueryDataResponse, error) {
	resp := &QueryDataResponse{Responses: make(map[string]DataResponse)}

	for _, q := range req.Queries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		fmt.Printf("    [Prometheus] 쿼리 실행: RefID=%s, Expr=%s\n", q.RefID, q.Expr)

		// 시계열 데이터 생성 (시뮬레이션)
		frames := generateTimeSeries(
			req.TimeRange.From,
			req.TimeRange.To,
			q.Expr,
			[]map[string]string{
				{"pod": "grafana-0", "namespace": "monitoring"},
				{"pod": "grafana-1", "namespace": "monitoring"},
			},
		)

		resp.Responses[q.RefID] = DataResponse{
			Frames: frames,
			Status: 200,
		}

		// 쿼리 실행 시간 시뮬레이션
		time.Sleep(50 * time.Millisecond)
	}

	return resp, nil
}

// ---------------------------------------------------------------------------
// 5. Loki 데이터소스 시뮬레이션
// ---------------------------------------------------------------------------

type lokiDS struct {
	uid string
	url string
}

func (l *lokiDS) PluginID() string { return "loki" }

func (l *lokiDS) QueryData(ctx context.Context, req *QueryDataRequest) (*QueryDataResponse, error) {
	resp := &QueryDataResponse{Responses: make(map[string]DataResponse)}

	for _, q := range req.Queries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		fmt.Printf("    [Loki] 쿼리 실행: RefID=%s, Expr=%s\n", q.RefID, q.Expr)

		// 로그 데이터 생성 (시뮬레이션)
		frame := generateLogData(req.TimeRange.From, req.TimeRange.To, q.Expr)

		resp.Responses[q.RefID] = DataResponse{
			Frames: []*DataFrame{frame},
			Status: 200,
		}

		time.Sleep(30 * time.Millisecond)
	}

	return resp, nil
}

// ---------------------------------------------------------------------------
// 6. 데이터 생성 헬퍼 함수
// ---------------------------------------------------------------------------

func generateTimeSeries(from, to time.Time, expr string, labelSets []map[string]string) []*DataFrame {
	frames := make([]*DataFrame, 0, len(labelSets))
	step := to.Sub(from) / 10 // 10개 데이터 포인트

	for _, labels := range labelSets {
		timeField := &Field{
			Name:   "time",
			Type:   FieldTypeTime,
			Values: make([]interface{}, 0, 10),
		}
		valueField := &Field{
			Name:   "value",
			Type:   FieldTypeFloat64,
			Values: make([]interface{}, 0, 10),
			Labels: labels,
		}

		baseValue := rand.Float64() * 50
		for i := 0; i < 10; i++ {
			t := from.Add(step * time.Duration(i))
			v := baseValue + math.Sin(float64(i)*0.5)*10 + rand.Float64()*5
			timeField.Values = append(timeField.Values, t)
			valueField.Values = append(valueField.Values, v)
		}

		frame := &DataFrame{
			Name:   labels["pod"],
			Fields: []*Field{timeField, valueField},
			Meta: &DataFrameMeta{
				ExecutedQueryString: expr,
			},
		}
		frames = append(frames, frame)
	}
	return frames
}

func generateLogData(from, to time.Time, expr string) *DataFrame {
	logMessages := []string{
		"level=info msg=\"HTTP request\" method=GET path=/api/dashboards status=200",
		"level=warn msg=\"Slow query\" duration=2.5s datasource=prometheus",
		"level=error msg=\"Connection refused\" host=localhost:9090",
		"level=info msg=\"Dashboard saved\" uid=abc-123 version=3",
		"level=debug msg=\"Cache hit\" key=dashboard:abc-123",
	}

	timeField := &Field{Name: "time", Type: FieldTypeTime, Values: make([]interface{}, 0)}
	lineField := &Field{Name: "line", Type: FieldTypeString, Values: make([]interface{}, 0)}

	step := to.Sub(from) / time.Duration(len(logMessages))
	for i, msg := range logMessages {
		t := from.Add(step * time.Duration(i))
		timeField.Values = append(timeField.Values, t)
		lineField.Values = append(lineField.Values, msg)
	}

	return &DataFrame{
		Name:   "logs",
		Fields: []*Field{timeField, lineField},
		Meta:   &DataFrameMeta{ExecutedQueryString: expr},
	}
}

// ---------------------------------------------------------------------------
// 7. QueryService — 쿼리 파이프라인 오케스트레이터
// ---------------------------------------------------------------------------

// QueryService — Grafana의 query.Service에 해당
type QueryService struct {
	plugins map[string]DataSourcePlugin // datasourceUID → plugin
}

// NewQueryService — 쿼리 서비스 생성
func NewQueryService() *QueryService {
	return &QueryService{
		plugins: make(map[string]DataSourcePlugin),
	}
}

// RegisterPlugin — 데이터소스 플러그인 등록
func (s *QueryService) RegisterPlugin(uid string, plugin DataSourcePlugin) {
	s.plugins[uid] = plugin
}

// QueryData — 메인 쿼리 실행 엔트리포인트
func (s *QueryService) QueryData(ctx context.Context, req *QueryDataRequest) (*QueryDataResponse, error) {
	fmt.Println("\n  [QueryService] 쿼리 파이프라인 시작")

	// 1. 쿼리를 데이터소스별로 그룹핑
	grouped := s.groupByDatasource(req.Queries)
	fmt.Printf("  [QueryService] %d개 데이터소스로 그룹핑됨\n", len(grouped))

	// 2. 타임아웃 컨텍스트 설정
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 3. 데이터소스별 병렬 쿼리 실행
	finalResp := &QueryDataResponse{Responses: make(map[string]DataResponse)}
	mu := &sync.Mutex{}
	wg := &sync.WaitGroup{}
	var firstErr error

	for dsUID, queries := range grouped {
		plugin, ok := s.plugins[dsUID]
		if !ok {
			for _, q := range queries {
				finalResp.Responses[q.RefID] = DataResponse{
					Error:  fmt.Errorf("데이터소스를 찾을 수 없음: %s", dsUID),
					Status: 400,
				}
			}
			continue
		}

		wg.Add(1)
		go func(uid string, p DataSourcePlugin, qs []DataQuery) {
			defer wg.Done()

			fmt.Printf("  [QueryService] 데이터소스 %s (%s) 쿼리 시작...\n", uid, p.PluginID())

			dsReq := &QueryDataRequest{
				Queries:   qs,
				TimeRange: req.TimeRange,
				Headers:   req.Headers,
			}

			resp, err := p.QueryData(queryCtx, dsReq)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				for _, q := range qs {
					finalResp.Responses[q.RefID] = DataResponse{
						Error:  err,
						Status: 500,
					}
				}
				mu.Unlock()
				return
			}

			mu.Lock()
			for refID, dr := range resp.Responses {
				finalResp.Responses[refID] = dr
			}
			mu.Unlock()
		}(dsUID, plugin, queries)
	}

	wg.Wait()
	fmt.Println("  [QueryService] 모든 쿼리 완료")

	return finalResp, firstErr
}

// groupByDatasource — 쿼리를 데이터소스 UID별로 그룹핑
func (s *QueryService) groupByDatasource(queries []DataQuery) map[string][]DataQuery {
	grouped := make(map[string][]DataQuery)
	for _, q := range queries {
		grouped[q.DatasourceUID] = append(grouped[q.DatasourceUID], q)
	}
	return grouped
}

// ---------------------------------------------------------------------------
// 8. DataFrame 출력 헬퍼
// ---------------------------------------------------------------------------

func printDataFrame(df *DataFrame) {
	if df.Meta != nil {
		fmt.Printf("      Query: %s\n", df.Meta.ExecutedQueryString)
	}
	fmt.Printf("      Frame: %s (%d rows)\n", df.Name, df.Rows())

	// 헤더
	headers := []string{}
	for _, f := range df.Fields {
		name := f.Name
		if len(f.Labels) > 0 {
			parts := []string{}
			for k, v := range f.Labels {
				parts = append(parts, fmt.Sprintf("%s=%s", k, v))
			}
			name += "{" + strings.Join(parts, ",") + "}"
		}
		headers = append(headers, fmt.Sprintf("%-28s", name))
	}
	fmt.Printf("      %s\n", strings.Join(headers, " | "))
	fmt.Printf("      %s\n", strings.Repeat("-", len(headers)*31))

	// 데이터 (최대 5행)
	maxRows := df.Rows()
	if maxRows > 5 {
		maxRows = 5
	}
	for i := 0; i < maxRows; i++ {
		row := []string{}
		for _, f := range df.Fields {
			switch v := f.Values[i].(type) {
			case time.Time:
				row = append(row, fmt.Sprintf("%-28s", v.Format("15:04:05")))
			case float64:
				row = append(row, fmt.Sprintf("%-28.2f", v))
			case string:
				s := v
				if len(s) > 28 {
					s = s[:25] + "..."
				}
				row = append(row, fmt.Sprintf("%-28s", s))
			default:
				row = append(row, fmt.Sprintf("%-28v", v))
			}
		}
		fmt.Printf("      %s\n", strings.Join(row, " | "))
	}
	if df.Rows() > 5 {
		fmt.Printf("      ... (%d rows more)\n", df.Rows()-5)
	}
}

// ---------------------------------------------------------------------------
// 9. 메인
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("==================================================")
	fmt.Println("  Grafana Query Pipeline Simulation")
	fmt.Println("==================================================")

	// 쿼리 서비스 초기화
	queryService := NewQueryService()

	// 데이터소스 플러그인 등록
	queryService.RegisterPlugin("prom-001", &prometheusDS{uid: "prom-001", url: "http://prometheus:9090"})
	queryService.RegisterPlugin("loki-001", &lokiDS{uid: "loki-001", url: "http://loki:3100"})

	fmt.Println("\n--- 데이터소스 플러그인 등록됨 ---")
	fmt.Println("  prom-001: Prometheus (http://prometheus:9090)")
	fmt.Println("  loki-001: Loki (http://loki:3100)")

	// 쿼리 요청 구성 (패널 3개: CPU, Memory from Prometheus + Logs from Loki)
	now := time.Now()
	req := &QueryDataRequest{
		Queries: []DataQuery{
			{
				RefID:         "A",
				DatasourceUID: "prom-001",
				QueryType:     "range",
				Expr:          `rate(container_cpu_usage_seconds_total{namespace="monitoring"}[5m])`,
				IntervalMs:    15000,
				MaxDataPoints: 1000,
			},
			{
				RefID:         "B",
				DatasourceUID: "prom-001",
				QueryType:     "range",
				Expr:          `container_memory_working_set_bytes{namespace="monitoring"}`,
				IntervalMs:    15000,
				MaxDataPoints: 1000,
			},
			{
				RefID:         "C",
				DatasourceUID: "loki-001",
				QueryType:     "range",
				Expr:          `{app="grafana"} |= "error"`,
				IntervalMs:    15000,
				MaxDataPoints: 1000,
			},
		},
		TimeRange: TimeRange{
			From: now.Add(-6 * time.Hour),
			To:   now,
		},
		Headers: map[string]string{
			"X-Grafana-Org-Id": "1",
		},
	}

	fmt.Println("\n--- 쿼리 요청 ---")
	for _, q := range req.Queries {
		fmt.Printf("  RefID=%s, DS=%s, Expr=%s\n", q.RefID, q.DatasourceUID, q.Expr)
	}

	// 쿼리 실행
	fmt.Println("\n--- 쿼리 파이프라인 실행 ---")
	startTime := time.Now()
	resp, err := queryService.QueryData(context.Background(), req)
	elapsed := time.Since(startTime)

	if err != nil {
		fmt.Printf("\n  쿼리 실행 에러: %v\n", err)
	}

	// 결과 출력
	fmt.Printf("\n--- 쿼리 결과 (소요: %v) ---\n", elapsed)
	for _, refID := range []string{"A", "B", "C"} {
		dr, ok := resp.Responses[refID]
		if !ok {
			continue
		}

		fmt.Printf("\n  [RefID=%s]", refID)
		if dr.Error != nil {
			fmt.Printf(" 에러: %v\n", dr.Error)
			continue
		}
		fmt.Printf(" Status=%d, Frames=%d\n", dr.Status, len(dr.Frames))

		for _, frame := range dr.Frames {
			printDataFrame(frame)
		}
	}

	// 타임아웃 시뮬레이션
	fmt.Println("\n--- 타임아웃 시뮬레이션 ---")
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // 타임아웃 발생 유도

	_, err = queryService.QueryData(timeoutCtx, req)
	if err != nil {
		fmt.Printf("  타임아웃 발생: %v\n", err)
	}

	// 파이프라인 아키텍처 요약
	fmt.Println("\n--- Grafana 쿼리 파이프라인 아키텍처 ---")
	fmt.Println(`
  ┌─────────┐     ┌──────────────┐     ┌─────────────────┐
  │ Browser │────▶│ /api/ds/query│────▶│  QueryService   │
  │ (Panel) │     │   (POST)     │     │  .QueryData()   │
  └─────────┘     └──────────────┘     └────────┬────────┘
                                                │
                         ┌──────────────────────┼──────────────────────┐
                         │                      │                      │
                         ▼                      ▼                      ▼
                  ┌─────────────┐       ┌─────────────┐       ┌─────────────┐
                  │ Prometheus  │       │    Loki     │       │   MySQL     │
                  │  Plugin     │       │   Plugin    │       │  Plugin     │
                  │ .QueryData()│       │ .QueryData()│       │ .QueryData()│
                  └──────┬──────┘       └──────┬──────┘       └──────┬──────┘
                         │                      │                      │
                         ▼                      ▼                      ▼
                  ┌─────────────┐       ┌─────────────┐       ┌─────────────┐
                  │  DataFrame  │       │  DataFrame  │       │  DataFrame  │
                  │ [Time,Value]│       │ [Time,Line] │       │ [Col1,Col2] │
                  └─────────────┘       └─────────────┘       └─────────────┘
                         │                      │                      │
                         └──────────────────────┼──────────────────────┘
                                                │
                                                ▼
                                    ┌───────────────────────┐
                                    │ QueryDataResponse     │
                                    │ { "A": [...frames],   │
                                    │   "B": [...frames],   │
                                    │   "C": [...frames] }  │
                                    └───────────────────────┘
`)
}
