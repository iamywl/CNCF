package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Prometheus HTTP API Server PoC
// =============================================================================
// 실제 Prometheus web/api/v1/api.go의 핵심 구조를 재현한다.
//
// 핵심 설계 포인트:
// 1. Response 봉투 패턴: {"status":"success/error","data":...} 통일 형식
// 2. apiFunc 패턴: 각 핸들러가 (data, error)를 반환 → wrap()이 JSON 직렬화
// 3. 라우팅: /api/v1/query, /api/v1/query_range, /api/v1/series 등
// 4. 에러 분류: bad_data, execution, timeout 등 errorType 체계
// 5. PromQL 파싱: metric{label="value"} 형태의 간단한 셀렉터 파싱
//
// 참조: prometheus/web/api/v1/api.go
//   - type Response struct { Status, Data, ErrorType, Error }
//   - func (api *API) Register(r *route.Router) - 라우트 등록
//   - func (api *API) query(r *http.Request) apiFuncResult - instant query
//   - func (api *API) queryRange(r *http.Request) apiFuncResult - range query
//   - func (api *API) series(r *http.Request) apiFuncResult - 시리즈 조회
//   - func (api *API) labelNames(r *http.Request) apiFuncResult - 레이블 이름
//   - func (api *API) labelValues(r *http.Request) apiFuncResult - 레이블 값
// =============================================================================

// ---------------------------------------------------------------------------
// 1. 응답 타입 정의 (prometheus/web/api/v1/api.go의 Response 구조)
// ---------------------------------------------------------------------------
// Prometheus API는 모든 응답을 동일한 봉투(envelope) 형식으로 감싼다.
// status가 "success"이면 data 필드에 결과가, "error"이면 errorType/error에 정보가 담긴다.

type status string

const (
	statusSuccess status = "success"
	statusError   status = "error"
)

// Response는 Prometheus HTTP API의 표준 응답 봉투이다.
// 실제 코드: web/api/v1/api.go의 Response struct
type Response struct {
	Status    status      `json:"status"`
	Data      interface{} `json:"data,omitempty"`
	ErrorType string      `json:"errorType,omitempty"`
	Error     string      `json:"error,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"`
}

// errorType은 API 에러의 분류 체계이다.
// 실제 코드에서는 errorNum(int) + errorType{num, str} 구조를 사용한다.
type errorType string

const (
	errorNone     errorType = ""
	errorBadData  errorType = "bad_data"
	errorExec     errorType = "execution"
	errorInternal errorType = "internal"
	errorTimeout  errorType = "timeout"
)

// apiError는 핸들러에서 반환하는 에러 구조체이다.
// 실제 코드: web/api/v1/api.go의 apiError struct
type apiError struct {
	typ errorType
	err error
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s: %s", e.typ, e.err)
}

// ---------------------------------------------------------------------------
// 2. 쿼리 결과 타입 (prometheus/promql/parser/value.go 참조)
// ---------------------------------------------------------------------------

// QueryData는 쿼리 응답의 data 필드 구조이다.
// 실제 코드: web/api/v1/api.go의 QueryData struct
type QueryData struct {
	ResultType string      `json:"resultType"`
	Result     interface{} `json:"result"`
}

// Sample은 즉시 쿼리 결과의 단일 샘플이다.
type Sample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]interface{}    `json:"value"` // [timestamp, "value_string"]
}

// RangeSample은 범위 쿼리 결과의 시리즈이다.
type RangeSample struct {
	Metric map[string]string `json:"metric"`
	Values [][2]interface{}  `json:"values"` // [[timestamp, "value_string"], ...]
}

// SeriesResult는 /api/v1/series 응답의 단일 시리즈 메타데이터이다.
type SeriesResult map[string]string

// ---------------------------------------------------------------------------
// 3. 인메모리 시계열 저장소
// ---------------------------------------------------------------------------

// TimeSeries는 하나의 시계열을 나타낸다.
type TimeSeries struct {
	Labels map[string]string // __name__, job, instance 등
	Points []DataPoint       // 시간순 데이터 포인트
}

// DataPoint는 단일 데이터 포인트이다.
type DataPoint struct {
	Timestamp float64 // Unix epoch seconds
	Value     float64
}

// Storage는 인메모리 시계열 저장소이다.
type Storage struct {
	mu     sync.RWMutex
	series []*TimeSeries
}

// NewStorage는 데모용 데이터가 미리 채워진 저장소를 생성한다.
func NewStorage() *Storage {
	s := &Storage{}
	now := float64(time.Now().Unix())

	// CPU 사용률 시계열 (4개 인스턴스)
	instances := []string{"server-01:9100", "server-02:9100", "server-03:9100", "server-04:9100"}
	cpuModes := []string{"user", "system", "idle"}
	for _, inst := range instances {
		for _, mode := range cpuModes {
			ts := &TimeSeries{
				Labels: map[string]string{
					"__name__": "node_cpu_seconds_total",
					"job":      "node-exporter",
					"instance": inst,
					"mode":     mode,
				},
			}
			baseVal := 100.0
			if mode == "idle" {
				baseVal = 5000.0
			}
			// 지난 1시간 데이터 (15초 간격)
			for t := now - 3600; t <= now; t += 15 {
				ts.Points = append(ts.Points, DataPoint{
					Timestamp: t,
					Value:     baseVal + rand.Float64()*10,
				})
			}
			s.series = append(s.series, ts)
		}
	}

	// HTTP 요청 카운터 시계열
	httpCodes := []string{"200", "404", "500"}
	httpMethods := []string{"GET", "POST"}
	for _, code := range httpCodes {
		for _, method := range httpMethods {
			ts := &TimeSeries{
				Labels: map[string]string{
					"__name__": "http_requests_total",
					"job":      "api-server",
					"instance": "api-01:8080",
					"code":     code,
					"method":   method,
				},
			}
			baseVal := 1000.0
			if code == "404" {
				baseVal = 50.0
			} else if code == "500" {
				baseVal = 5.0
			}
			for t := now - 3600; t <= now; t += 15 {
				ts.Points = append(ts.Points, DataPoint{
					Timestamp: t,
					Value:     baseVal + float64(int(t-now+3600)/15),
				})
			}
			s.series = append(s.series, ts)
		}
	}

	// Go 메모리 통계 시계열
	ts := &TimeSeries{
		Labels: map[string]string{
			"__name__": "go_memstats_alloc_bytes",
			"job":      "prometheus",
			"instance": "localhost:9090",
		},
	}
	for t := now - 3600; t <= now; t += 15 {
		ts.Points = append(ts.Points, DataPoint{
			Timestamp: t,
			Value:     50*1024*1024 + rand.Float64()*10*1024*1024,
		})
	}
	s.series = append(s.series, ts)

	return s
}

// ---------------------------------------------------------------------------
// 4. 간단한 PromQL 셀렉터 파서
// ---------------------------------------------------------------------------
// 실제 Prometheus는 promql/parser 패키지에서 완전한 PromQL을 파싱한다.
// 여기서는 metric_name{label1="value1",label2="value2"} 형태만 지원한다.
// 매처 타입: = (equal), != (not equal), =~ (regex match), !~ (regex not match)

// Matcher는 레이블 매처이다.
// 실제 코드: model/labels/matcher.go
type Matcher struct {
	Name    string
	Value   string
	Type    MatchType
	re      *regexp.Regexp // =~, !~ 용
}

type MatchType int

const (
	MatchEqual        MatchType = iota // =
	MatchNotEqual                      // !=
	MatchRegexp                        // =~
	MatchNotRegexp                     // !~
)

// Matches는 주어진 값이 매처에 맞는지 확인한다.
func (m *Matcher) Matches(value string) bool {
	switch m.Type {
	case MatchEqual:
		return value == m.Value
	case MatchNotEqual:
		return value != m.Value
	case MatchRegexp:
		if m.re == nil {
			m.re, _ = regexp.Compile("^(?:" + m.Value + ")$")
		}
		return m.re != nil && m.re.MatchString(value)
	case MatchNotRegexp:
		if m.re == nil {
			m.re, _ = regexp.Compile("^(?:" + m.Value + ")$")
		}
		return m.re == nil || !m.re.MatchString(value)
	}
	return false
}

// ParseSelector는 "metric{label=value,...}" 문자열을 파싱한다.
func ParseSelector(input string) (string, []*Matcher, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", nil, fmt.Errorf("empty query")
	}

	var metricName string
	var matcherStr string

	braceIdx := strings.Index(input, "{")
	if braceIdx == -1 {
		// 중괄호 없음 → 메트릭 이름만
		metricName = input
	} else {
		metricName = strings.TrimSpace(input[:braceIdx])
		endBrace := strings.LastIndex(input, "}")
		if endBrace == -1 {
			return "", nil, fmt.Errorf("missing closing brace in selector: %s", input)
		}
		matcherStr = input[braceIdx+1 : endBrace]
	}

	var matchers []*Matcher

	// __name__ 매처 추가
	if metricName != "" {
		matchers = append(matchers, &Matcher{
			Name:  "__name__",
			Value: metricName,
			Type:  MatchEqual,
		})
	}

	// 레이블 매처 파싱
	if matcherStr != "" {
		parts := splitMatchers(matcherStr)
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			m, err := parseSingleMatcher(part)
			if err != nil {
				return "", nil, err
			}
			matchers = append(matchers, m)
		}
	}

	if len(matchers) == 0 {
		return "", nil, fmt.Errorf("no matchers provided")
	}

	return metricName, matchers, nil
}

// splitMatchers는 따옴표 안의 콤마를 무시하고 매처를 분리한다.
func splitMatchers(s string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false
	escaped := false

	for _, ch := range s {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			current.WriteRune(ch)
			continue
		}
		if ch == '"' {
			inQuotes = !inQuotes
			current.WriteRune(ch)
			continue
		}
		if ch == ',' && !inQuotes {
			parts = append(parts, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(ch)
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// parseSingleMatcher는 단일 매처 (name="value") 를 파싱한다.
func parseSingleMatcher(s string) (*Matcher, error) {
	// 연산자 찾기: =~, !~, !=, =
	var opIdx int
	var matchType MatchType
	var opLen int

	for i := 0; i < len(s); i++ {
		if i+1 < len(s) {
			twoChar := s[i : i+2]
			switch twoChar {
			case "=~":
				opIdx = i
				matchType = MatchRegexp
				opLen = 2
				goto found
			case "!~":
				opIdx = i
				matchType = MatchNotRegexp
				opLen = 2
				goto found
			case "!=":
				opIdx = i
				matchType = MatchNotEqual
				opLen = 2
				goto found
			}
		}
		if s[i] == '=' {
			opIdx = i
			matchType = MatchEqual
			opLen = 1
			goto found
		}
	}
	return nil, fmt.Errorf("no operator found in matcher: %s", s)

found:
	name := strings.TrimSpace(s[:opIdx])
	value := strings.TrimSpace(s[opIdx+opLen:])
	// 따옴표 제거
	value = strings.Trim(value, "\"'")

	return &Matcher{
		Name:  name,
		Value: value,
		Type:  matchType,
	}, nil
}

// ---------------------------------------------------------------------------
// 5. 쿼리 실행 엔진
// ---------------------------------------------------------------------------

// Query는 저장소에서 매처에 맞는 시리즈를 찾는다.
func (s *Storage) Query(matchers []*Matcher) []*TimeSeries {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []*TimeSeries
	for _, ts := range s.series {
		if matchesSeries(ts, matchers) {
			results = append(results, ts)
		}
	}
	return results
}

// matchesSeries는 시리즈가 모든 매처를 만족하는지 확인한다.
func matchesSeries(ts *TimeSeries, matchers []*Matcher) bool {
	for _, m := range matchers {
		labelValue, exists := ts.Labels[m.Name]
		if !exists {
			// 레이블이 존재하지 않으면 빈 문자열로 매칭
			labelValue = ""
		}
		if !m.Matches(labelValue) {
			return false
		}
	}
	return true
}

// InstantQuery는 특정 시간의 값을 반환한다 (가장 가까운 포인트).
func (s *Storage) InstantQuery(matchers []*Matcher, ts float64) []Sample {
	series := s.Query(matchers)
	var samples []Sample

	for _, ser := range series {
		// ts에 가장 가까운 포인트 찾기 (5분 이내)
		var closest *DataPoint
		minDist := math.MaxFloat64
		for i := range ser.Points {
			dist := math.Abs(ser.Points[i].Timestamp - ts)
			if dist < minDist && dist <= 300 { // 5분 lookback
				minDist = dist
				closest = &ser.Points[i]
			}
		}
		if closest != nil {
			// 실제 Prometheus는 [unix_timestamp, "string_value"] 형식으로 반환
			samples = append(samples, Sample{
				Metric: copyLabels(ser.Labels),
				Value:  [2]interface{}{closest.Timestamp, formatFloat(closest.Value)},
			})
		}
	}
	return samples
}

// RangeQuery는 시간 범위의 값을 반환한다.
func (s *Storage) RangeQuery(matchers []*Matcher, start, end, step float64) []RangeSample {
	series := s.Query(matchers)
	var results []RangeSample

	for _, ser := range series {
		var values [][2]interface{}
		for t := start; t <= end; t += step {
			// 각 step 시간에 가장 가까운 포인트 찾기
			var closest *DataPoint
			minDist := math.MaxFloat64
			for i := range ser.Points {
				dist := math.Abs(ser.Points[i].Timestamp - t)
				if dist < minDist && dist <= 300 {
					minDist = dist
					closest = &ser.Points[i]
				}
			}
			if closest != nil {
				values = append(values, [2]interface{}{t, formatFloat(closest.Value)})
			}
		}
		if len(values) > 0 {
			results = append(results, RangeSample{
				Metric: copyLabels(ser.Labels),
				Values: values,
			})
		}
	}
	return results
}

// AllLabelNames는 모든 레이블 이름을 반환한다.
func (s *Storage) AllLabelNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nameSet := make(map[string]struct{})
	for _, ts := range s.series {
		for k := range ts.Labels {
			nameSet[k] = struct{}{}
		}
	}
	names := make([]string, 0, len(nameSet))
	for k := range nameSet {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// LabelValues는 특정 레이블의 고유 값들을 반환한다.
func (s *Storage) LabelValues(name string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	valueSet := make(map[string]struct{})
	for _, ts := range s.series {
		if v, ok := ts.Labels[name]; ok {
			valueSet[v] = struct{}{}
		}
	}
	values := make([]string, 0, len(valueSet))
	for v := range valueSet {
		values = append(values, v)
	}
	sort.Strings(values)
	return values
}

// SeriesMetadata는 매처에 맞는 시리즈의 레이블 셋을 반환한다.
func (s *Storage) SeriesMetadata(matchers []*Matcher) []SeriesResult {
	matched := s.Query(matchers)
	var results []SeriesResult
	for _, ts := range matched {
		results = append(results, SeriesResult(copyLabels(ts.Labels)))
	}
	return results
}

func copyLabels(m map[string]string) map[string]string {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ---------------------------------------------------------------------------
// 6. HTTP API 서버 (prometheus/web/api/v1/api.go의 API 구조 재현)
// ---------------------------------------------------------------------------

// API는 Prometheus HTTP API 서버이다.
// 실제 코드의 API struct가 Queryable, QueryEngine 등을 보유하는 것처럼,
// 여기서는 Storage를 보유한다.
type API struct {
	storage *Storage
	ready   bool
}

// apiFuncResult는 핸들러 함수의 반환 타입이다.
// 실제 코드: web/api/v1/api.go의 apiFuncResult struct
type apiFuncResult struct {
	data interface{}
	err  *apiError
}

// apiFunc는 핸들러 함수 시그니처이다.
// 실제 코드: type apiFunc func(r *http.Request) apiFuncResult
type apiFunc func(r *http.Request) apiFuncResult

// Register는 API 엔드포인트를 등록한다.
// 실제 코드: func (api *API) Register(r *route.Router)
// Prometheus는 github.com/prometheus/common/route를 사용하지만,
// 여기서는 표준 net/http의 ServeMux를 사용한다.
func (api *API) Register(mux *http.ServeMux) {
	// wrap 함수: apiFunc → http.HandlerFunc 변환
	// 실제 코드의 wrap()과 동일한 패턴: 결과를 JSON 응답으로 직렬화
	wrap := func(f apiFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			result := f(r)

			w.Header().Set("Content-Type", "application/json")

			if result.err != nil {
				// 에러 응답
				resp := Response{
					Status:    statusError,
					ErrorType: string(result.err.typ),
					Error:     result.err.err.Error(),
				}
				if result.err.typ == errorBadData {
					w.WriteHeader(http.StatusBadRequest)
				} else if result.err.typ == errorInternal {
					w.WriteHeader(http.StatusInternalServerError)
				} else {
					w.WriteHeader(http.StatusUnprocessableEntity)
				}
				json.NewEncoder(w).Encode(resp)
				return
			}

			resp := Response{
				Status: statusSuccess,
				Data:   result.data,
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(resp)
		}
	}

	// readyWrap: readiness 체크를 추가하는 래퍼
	// 실제 코드: api.ready(handler) 패턴
	readyWrap := func(f apiFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !api.ready {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(Response{
					Status:    statusError,
					ErrorType: "unavailable",
					Error:     "Service Unavailable",
				})
				return
			}
			wrap(f)(w, r)
		}
	}

	// 라우트 등록 (실제 코드의 Register() 메서드 참조)
	mux.HandleFunc("/api/v1/query", readyWrap(api.query))
	mux.HandleFunc("/api/v1/query_range", readyWrap(api.queryRange))
	mux.HandleFunc("/api/v1/series", readyWrap(api.series))
	mux.HandleFunc("/api/v1/labels", readyWrap(api.labelNames))
	// label values는 URL 패턴에서 이름 추출 필요 → 별도 처리
	mux.HandleFunc("/api/v1/label/", readyWrap(api.labelValues))

	// 헬스/레디 엔드포인트 (실제 코드: web/web.go)
	mux.HandleFunc("/-/healthy", api.healthy)
	mux.HandleFunc("/-/ready", api.readyCheck)
}

// ---------------------------------------------------------------------------
// 7. 각 API 핸들러 구현
// ---------------------------------------------------------------------------

// query는 즉시 쿼리(instant query)를 처리한다.
// 실제 코드: func (api *API) query(r *http.Request) apiFuncResult
// GET /api/v1/query?query=metric{label="value"}&time=<unix_timestamp>
func (api *API) query(r *http.Request) apiFuncResult {
	queryExpr := r.URL.Query().Get("query")
	if queryExpr == "" {
		return apiFuncResult{nil, &apiError{
			typ: errorBadData,
			err: fmt.Errorf("invalid parameter \"query\": empty query"),
		}}
	}

	// 시간 파라미터 파싱 (기본값: 현재 시간)
	var ts float64
	timeStr := r.URL.Query().Get("time")
	if timeStr != "" {
		var err error
		ts, err = strconv.ParseFloat(timeStr, 64)
		if err != nil {
			return apiFuncResult{nil, &apiError{
				typ: errorBadData,
				err: fmt.Errorf("invalid parameter \"time\": %v", err),
			}}
		}
	} else {
		ts = float64(time.Now().Unix())
	}

	// PromQL 셀렉터 파싱
	_, matchers, err := ParseSelector(queryExpr)
	if err != nil {
		return apiFuncResult{nil, &apiError{
			typ: errorBadData,
			err: fmt.Errorf("invalid parameter \"query\": %v", err),
		}}
	}

	// 즉시 쿼리 실행
	samples := api.storage.InstantQuery(matchers, ts)

	return apiFuncResult{
		data: QueryData{
			ResultType: "vector",
			Result:     samples,
		},
	}
}

// queryRange는 범위 쿼리(range query)를 처리한다.
// 실제 코드: func (api *API) queryRange(r *http.Request) apiFuncResult
// GET /api/v1/query_range?query=...&start=...&end=...&step=...
func (api *API) queryRange(r *http.Request) apiFuncResult {
	queryExpr := r.URL.Query().Get("query")
	if queryExpr == "" {
		return apiFuncResult{nil, &apiError{
			typ: errorBadData,
			err: fmt.Errorf("invalid parameter \"query\": empty query"),
		}}
	}

	// 필수 파라미터 검증
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	stepStr := r.URL.Query().Get("step")

	if startStr == "" {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid parameter \"start\": missing start parameter")}}
	}
	if endStr == "" {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid parameter \"end\": missing end parameter")}}
	}
	if stepStr == "" {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid parameter \"step\": missing step parameter")}}
	}

	start, err := strconv.ParseFloat(startStr, 64)
	if err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid parameter \"start\": %v", err)}}
	}
	end, err := strconv.ParseFloat(endStr, 64)
	if err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid parameter \"end\": %v", err)}}
	}
	step, err := strconv.ParseFloat(stepStr, 64)
	if err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid parameter \"step\": %v", err)}}
	}

	// 실제 코드의 검증 로직 재현
	if end < start {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid parameter \"end\": end timestamp must not be before start time")}}
	}
	if step <= 0 {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid parameter \"step\": zero or negative query resolution step widths are not accepted")}}
	}
	// 최대 11,000 포인트 제한 (실제 코드 동일)
	if (end-start)/step > 11000 {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("exceeded maximum resolution of 11,000 points per timeseries")}}
	}

	_, matchers, err := ParseSelector(queryExpr)
	if err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid parameter \"query\": %v", err)}}
	}

	results := api.storage.RangeQuery(matchers, start, end, step)

	return apiFuncResult{
		data: QueryData{
			ResultType: "matrix",
			Result:     results,
		},
	}
}

// series는 시리즈 메타데이터를 반환한다.
// 실제 코드: func (api *API) series(r *http.Request) apiFuncResult
// GET /api/v1/series?match[]=metric{label="value"}
func (api *API) series(r *http.Request) apiFuncResult {
	matchParams := r.URL.Query()["match[]"]
	if len(matchParams) == 0 {
		return apiFuncResult{nil, &apiError{
			typ: errorBadData,
			err: fmt.Errorf("no match[] parameter provided"),
		}}
	}

	var allResults []SeriesResult
	for _, matchParam := range matchParams {
		_, matchers, err := ParseSelector(matchParam)
		if err != nil {
			return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid parameter \"match[]\": %v", err)}}
		}
		results := api.storage.SeriesMetadata(matchers)
		allResults = append(allResults, results...)
	}

	if allResults == nil {
		allResults = []SeriesResult{}
	}
	return apiFuncResult{data: allResults}
}

// labelNames는 모든 레이블 이름을 반환한다.
// 실제 코드: func (api *API) labelNames(r *http.Request) apiFuncResult
// GET /api/v1/labels
func (api *API) labelNames(r *http.Request) apiFuncResult {
	_ = r // 간단한 구현에서는 파라미터 무시
	names := api.storage.AllLabelNames()
	return apiFuncResult{data: names}
}

// labelValues는 특정 레이블의 값들을 반환한다.
// 실제 코드: func (api *API) labelValues(r *http.Request) apiFuncResult
// GET /api/v1/label/{name}/values
func (api *API) labelValues(r *http.Request) apiFuncResult {
	// URL에서 레이블 이름 추출: /api/v1/label/{name}/values
	path := r.URL.Path
	// /api/v1/label/ 이후의 부분에서 /values 이전까지
	prefix := "/api/v1/label/"
	suffix := "/values"

	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return apiFuncResult{nil, &apiError{
			typ: errorBadData,
			err: fmt.Errorf("invalid label value endpoint: %s", path),
		}}
	}

	name := path[len(prefix) : len(path)-len(suffix)]
	if name == "" {
		return apiFuncResult{nil, &apiError{
			typ: errorBadData,
			err: fmt.Errorf("invalid label name: empty"),
		}}
	}

	values := api.storage.LabelValues(name)
	if values == nil {
		values = []string{}
	}
	return apiFuncResult{data: values}
}

// healthy는 헬스 체크 엔드포인트이다.
// 실제 코드: web/web.go의 /-/healthy 핸들러
func (api *API) healthy(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Prometheus PoC is Healthy.\n")
}

// readyCheck는 레디니스 체크 엔드포인트이다.
// 실제 코드: web/web.go의 /-/ready 핸들러
func (api *API) readyCheck(w http.ResponseWriter, _ *http.Request) {
	if !api.ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "Prometheus PoC is Not Ready.\n")
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Prometheus PoC is Ready.\n")
}

// ---------------------------------------------------------------------------
// 8. 데모 실행
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("=== Prometheus HTTP API Server PoC ===")
	fmt.Println()
	fmt.Println("Prometheus HTTP API(web/api/v1/api.go)의 핵심 구조를 재현합니다.")
	fmt.Println("- Response 봉투 패턴: {\"status\":\"success\",\"data\":...}")
	fmt.Println("- apiFunc → wrap → JSON 직렬화 패턴")
	fmt.Println("- 에러 타입 분류 체계 (bad_data, execution, timeout 등)")
	fmt.Println()

	// 저장소 및 API 초기화
	storage := NewStorage()
	api := &API{
		storage: storage,
		ready:   true,
	}

	// HTTP 서버 설정
	mux := http.NewServeMux()
	api.Register(mux)

	// 랜덤 포트로 서버 시작
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("서버 시작 실패: %v\n", err)
		return
	}
	baseURL := fmt.Sprintf("http://%s", listener.Addr().String())

	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	fmt.Printf("서버 시작: %s\n", baseURL)
	fmt.Println()

	client := &http.Client{Timeout: 5 * time.Second}
	now := float64(time.Now().Unix())

	// -----------------------------------------------------------------------
	// 테스트 1: 헬스 체크
	// -----------------------------------------------------------------------
	fmt.Println("--- 1. 헬스 체크: GET /-/healthy ---")
	doTextRequest(client, baseURL+"/-/healthy")

	// -----------------------------------------------------------------------
	// 테스트 2: 레디니스 체크
	// -----------------------------------------------------------------------
	fmt.Println("--- 2. 레디니스 체크: GET /-/ready ---")
	doTextRequest(client, baseURL+"/-/ready")

	// -----------------------------------------------------------------------
	// 테스트 3: Instant Query
	// -----------------------------------------------------------------------
	fmt.Println("--- 3. Instant Query: GET /api/v1/query ---")
	fmt.Println("    쿼리: http_requests_total{code=\"200\",method=\"GET\"}")
	url := fmt.Sprintf("%s/api/v1/query?query=%s&time=%s",
		baseURL,
		"http_requests_total{code=\"200\",method=\"GET\"}",
		strconv.FormatFloat(now, 'f', 0, 64),
	)
	doJSONRequest(client, url)

	// -----------------------------------------------------------------------
	// 테스트 4: Range Query
	// -----------------------------------------------------------------------
	fmt.Println("--- 4. Range Query: GET /api/v1/query_range ---")
	fmt.Println("    쿼리: go_memstats_alloc_bytes (최근 5분, 60초 간격)")
	url = fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%s&end=%s&step=%s",
		baseURL,
		"go_memstats_alloc_bytes",
		strconv.FormatFloat(now-300, 'f', 0, 64),
		strconv.FormatFloat(now, 'f', 0, 64),
		"60",
	)
	doJSONRequest(client, url)

	// -----------------------------------------------------------------------
	// 테스트 5: Series 조회
	// -----------------------------------------------------------------------
	fmt.Println("--- 5. Series 조회: GET /api/v1/series ---")
	fmt.Println("    매처: node_cpu_seconds_total{instance=\"server-01:9100\"}")
	url = fmt.Sprintf("%s/api/v1/series?match[]=%s",
		baseURL,
		"node_cpu_seconds_total{instance=\"server-01:9100\"}",
	)
	doJSONRequest(client, url)

	// -----------------------------------------------------------------------
	// 테스트 6: Label Names
	// -----------------------------------------------------------------------
	fmt.Println("--- 6. Label Names: GET /api/v1/labels ---")
	doJSONRequest(client, baseURL+"/api/v1/labels")

	// -----------------------------------------------------------------------
	// 테스트 7: Label Values
	// -----------------------------------------------------------------------
	fmt.Println("--- 7. Label Values: GET /api/v1/label/job/values ---")
	doJSONRequest(client, baseURL+"/api/v1/label/job/values")

	fmt.Println("--- 8. Label Values: GET /api/v1/label/instance/values ---")
	doJSONRequest(client, baseURL+"/api/v1/label/instance/values")

	// -----------------------------------------------------------------------
	// 테스트 9: 에러 응답 (빈 쿼리)
	// -----------------------------------------------------------------------
	fmt.Println("--- 9. 에러 응답: 빈 쿼리 ---")
	doJSONRequest(client, baseURL+"/api/v1/query")

	// -----------------------------------------------------------------------
	// 테스트 10: 에러 응답 (잘못된 셀렉터)
	// -----------------------------------------------------------------------
	fmt.Println("--- 10. 에러 응답: 잘못된 셀렉터 ---")
	url = fmt.Sprintf("%s/api/v1/query?query=%s", baseURL, "metric{bad}")
	doJSONRequest(client, url)

	// -----------------------------------------------------------------------
	// 테스트 11: Range Query 파라미터 검증
	// -----------------------------------------------------------------------
	fmt.Println("--- 11. 에러 응답: end < start ---")
	url = fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%s&end=%s&step=%s",
		baseURL,
		"http_requests_total",
		strconv.FormatFloat(now, 'f', 0, 64),
		strconv.FormatFloat(now-600, 'f', 0, 64),
		"60",
	)
	doJSONRequest(client, url)

	// -----------------------------------------------------------------------
	// 테스트 12: series match[] 누락
	// -----------------------------------------------------------------------
	fmt.Println("--- 12. 에러 응답: match[] 파라미터 누락 ---")
	doJSONRequest(client, baseURL+"/api/v1/series")

	fmt.Println("=== PoC 완료 ===")
}

// doJSONRequest는 HTTP GET 요청을 보내고 JSON 응답을 보기 좋게 출력한다.
func doJSONRequest(client *http.Client, url string) {
	resp, err := client.Get(url)
	if err != nil {
		fmt.Printf("  요청 실패: %v\n\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	fmt.Printf("  HTTP %d\n", resp.StatusCode)

	// JSON 포맷팅
	var prettyJSON map[string]interface{}
	if err := json.Unmarshal(body, &prettyJSON); err == nil {
		formatted, _ := json.MarshalIndent(prettyJSON, "  ", "  ")
		output := string(formatted)
		// 너무 길면 잘라서 표시
		lines := strings.Split(output, "\n")
		if len(lines) > 30 {
			for _, line := range lines[:25] {
				fmt.Printf("  %s\n", line)
			}
			fmt.Printf("  ... (총 %d줄, 나머지 생략)\n", len(lines))
		} else {
			fmt.Printf("  %s\n", output)
		}
	} else {
		fmt.Printf("  %s\n", string(body))
	}
	fmt.Println()
}

// doTextRequest는 HTTP GET 요청을 보내고 텍스트 응답을 출력한다.
func doTextRequest(client *http.Client, url string) {
	resp, err := client.Get(url)
	if err != nil {
		fmt.Printf("  요청 실패: %v\n\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("  HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
	fmt.Println()
}
