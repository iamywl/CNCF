// Package main은 Alertmanager의 API Metrics, pkg/labels 매처, types 패키지를
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. API 알림 메트릭 (firing/resolved/invalid 카운터)
// 2. HTTP 요청 메트릭 (InstrumentHandler 패턴)
// 3. Matcher 타입 (=, !=, =~, !~)
// 4. Matcher 파싱 (문자열 → Matcher 객체)
// 5. LabelSet 매칭 (다중 Matcher AND 조합)
// 6. Alert 타입 (StartsAt, EndsAt, Status 결정)
// 7. AlertStatus 타입 (Inhibited, Silenced, Active)
// 8. Silence 타입 (알림 억제)
// 9. Marker 인터페이스 (알림 상태 추적)
// 10. UTF-8 레이블 처리
//
// 실제 소스 참조:
//   - api/metrics/metrics.go   (API 메트릭)
//   - pkg/labels/matcher.go    (Matcher 타입)
//   - pkg/labels/parse.go      (Matcher 파싱)
//   - types/types.go           (Alert, AlertStatus)
//   - alert/alert.go           (Marker)
package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 1. API Metrics (api/metrics/metrics.go 시뮬레이션)
// ============================================================================

// AlertMetrics는 API를 통해 수신된 알림 메트릭이다.
type AlertMetrics struct {
	firing   atomic.Int64
	resolved atomic.Int64
	invalid  atomic.Int64
}

// NewAlertMetrics는 새 알림 메트릭을 생성한다.
func NewAlertMetrics() *AlertMetrics {
	return &AlertMetrics{}
}

// RecordFiring은 firing 알림을 기록한다.
func (m *AlertMetrics) RecordFiring()   { m.firing.Add(1) }
func (m *AlertMetrics) RecordResolved() { m.resolved.Add(1) }
func (m *AlertMetrics) RecordInvalid()  { m.invalid.Add(1) }

func (m *AlertMetrics) String() string {
	return fmt.Sprintf("alertmanager_alerts_received_total{status=\"firing\"} %d\n"+
		"alertmanager_alerts_received_total{status=\"resolved\"} %d\n"+
		"alertmanager_alerts_invalid_total %d",
		m.firing.Load(), m.resolved.Load(), m.invalid.Load())
}

// HTTPMetrics는 HTTP 요청 메트릭이다.
type HTTPMetrics struct {
	mu       sync.Mutex
	requests map[string]int64 // method:path:status → count
	duration map[string][]time.Duration
}

// NewHTTPMetrics는 새 HTTP 메트릭을 생성한다.
func NewHTTPMetrics() *HTTPMetrics {
	return &HTTPMetrics{
		requests: make(map[string]int64),
		duration: make(map[string][]time.Duration),
	}
}

// RecordRequest는 HTTP 요청을 기록한다.
func (m *HTTPMetrics) RecordRequest(method, path string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%s:%s:%d", method, path, status)
	m.requests[key]++
	m.duration[key] = append(m.duration[key], d)
}

func (m *HTTPMetrics) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var lines []string
	for key, count := range m.requests {
		parts := strings.SplitN(key, ":", 3)
		lines = append(lines, fmt.Sprintf(
			"alertmanager_http_request_duration_seconds_count{handler=\"%s\",method=\"%s\",code=\"%s\"} %d",
			parts[1], parts[0], parts[2], count))
	}
	return strings.Join(lines, "\n")
}

// ============================================================================
// 2. Matcher (pkg/labels/matcher.go 시뮬레이션)
// ============================================================================

// MatchType은 매처 유형이다.
type MatchType int

const (
	MatchEqual    MatchType = iota // =
	MatchNotEqual                  // !=
	MatchRegexp                    // =~
	MatchNotRegexp                 // !~
)

func (t MatchType) String() string {
	switch t {
	case MatchEqual:
		return "="
	case MatchNotEqual:
		return "!="
	case MatchRegexp:
		return "=~"
	case MatchNotRegexp:
		return "!~"
	default:
		return "?"
	}
}

// Matcher는 레이블 값을 매칭하는 규칙이다.
type Matcher struct {
	Name    string
	Type    MatchType
	Value   string
	re      *regexp.Regexp // 정규식 매처용
}

// NewMatcher는 새 Matcher를 생성한다.
func NewMatcher(name string, mt MatchType, value string) (*Matcher, error) {
	m := &Matcher{Name: name, Type: mt, Value: value}
	if mt == MatchRegexp || mt == MatchNotRegexp {
		re, err := regexp.Compile("^(?:" + value + ")$")
		if err != nil {
			return nil, fmt.Errorf("잘못된 정규식 %q: %w", value, err)
		}
		m.re = re
	}
	return m, nil
}

// Matches는 값이 매처와 일치하는지 확인한다.
func (m *Matcher) Matches(value string) bool {
	switch m.Type {
	case MatchEqual:
		return value == m.Value
	case MatchNotEqual:
		return value != m.Value
	case MatchRegexp:
		return m.re.MatchString(value)
	case MatchNotRegexp:
		return !m.re.MatchString(value)
	default:
		return false
	}
}

func (m *Matcher) String() string {
	return fmt.Sprintf("%s%s%q", m.Name, m.Type, m.Value)
}

// ============================================================================
// 3. Matcher 파싱 (pkg/labels/parse.go 시뮬레이션)
// ============================================================================

// ParseMatcher는 문자열에서 Matcher를 파싱한다.
// 형식: name=value, name!=value, name=~regex, name!~regex
func ParseMatcher(s string) (*Matcher, error) {
	s = strings.TrimSpace(s)

	var name string
	var mt MatchType
	var value string

	for i, operators := 0, []struct {
		op string
		mt MatchType
	}{
		{"!~", MatchNotRegexp},
		{"=~", MatchRegexp},
		{"!=", MatchNotEqual},
		{"=", MatchEqual},
	}; i < len(operators); i++ {
		idx := strings.Index(s, operators[i].op)
		if idx > 0 {
			name = strings.TrimSpace(s[:idx])
			mt = operators[i].mt
			value = strings.TrimSpace(s[idx+len(operators[i].op):])
			// 따옴표 제거
			value = strings.Trim(value, `"'`)
			break
		}
	}

	if name == "" {
		return nil, fmt.Errorf("잘못된 매처 형식: %q", s)
	}

	return NewMatcher(name, mt, value)
}

// ParseMatchers는 쉼표 구분 문자열에서 여러 Matcher를 파싱한다.
func ParseMatchers(s string) ([]*Matcher, error) {
	// {key=value, key2!=value2} 형식 처리
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")

	var matchers []*Matcher
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		m, err := ParseMatcher(part)
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, m)
	}
	return matchers, nil
}

// ============================================================================
// 4. LabelSet 매칭
// ============================================================================

// LabelSet은 레이블 집합이다.
type LabelSet map[string]string

// MatchesAll은 모든 Matcher가 LabelSet과 일치하는지 확인한다 (AND 조합).
func MatchesAll(labels LabelSet, matchers []*Matcher) bool {
	for _, m := range matchers {
		v := labels[m.Name]
		if !m.Matches(v) {
			return false
		}
	}
	return true
}

// ============================================================================
// 5. Alert 타입 (types/types.go 시뮬레이션)
// ============================================================================

// Alert는 Alertmanager의 핵심 알림 타입이다.
type Alert struct {
	Labels      LabelSet          `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
	GeneratorURL string           `json:"generatorURL"`
}

// Status는 알림의 현재 상태를 결정한다.
func (a *Alert) Status() string {
	if a.Resolved() {
		return "resolved"
	}
	return "firing"
}

// Resolved는 알림이 해결되었는지 확인한다.
func (a *Alert) Resolved() bool {
	return !a.EndsAt.IsZero() && a.EndsAt.Before(time.Now())
}

// Fingerprint는 알림의 고유 식별자를 생성한다 (레이블 기반).
func (a *Alert) Fingerprint() string {
	var parts []string
	for k, v := range a.Labels {
		parts = append(parts, k+"="+v)
	}
	// 간단한 해시 시뮬레이션
	return fmt.Sprintf("%x", hashString(strings.Join(parts, ",")))
}

func hashString(s string) uint64 {
	var h uint64
	for _, c := range s {
		h = h*31 + uint64(c)
	}
	return h
}

// ============================================================================
// 6. AlertStatus (types/types.go 시뮬레이션)
// ============================================================================

// AlertStatusState는 알림의 처리 상태다.
type AlertStatusState string

const (
	AlertStateActive    AlertStatusState = "active"
	AlertStateSuppressed AlertStatusState = "suppressed"
	AlertStateUnprocessed AlertStatusState = "unprocessed"
)

// AlertStatus는 알림의 처리 상태 정보다.
type AlertStatus struct {
	State       AlertStatusState
	InhibitedBy []string // 이 알림을 억제한 규칙 ID
	SilencedBy  []string // 이 알림을 소거한 Silence ID
}

// ============================================================================
// 7. Marker (alert/alert.go 시뮬레이션)
// ============================================================================

// Marker는 알림의 상태를 추적하는 인터페이스다.
type Marker interface {
	SetActive(fingerprint string)
	SetInhibited(fingerprint string, ids ...string)
	SetSilenced(fingerprint string, ids ...string)
	Status(fingerprint string) AlertStatus
}

// SimpleMarker는 Marker의 간단한 구현이다.
type SimpleMarker struct {
	mu       sync.Mutex
	statuses map[string]AlertStatus
}

// NewSimpleMarker는 새 Marker를 생성한다.
func NewSimpleMarker() *SimpleMarker {
	return &SimpleMarker{statuses: make(map[string]AlertStatus)}
}

func (m *SimpleMarker) SetActive(fp string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[fp] = AlertStatus{State: AlertStateActive}
}

func (m *SimpleMarker) SetInhibited(fp string, ids ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[fp] = AlertStatus{State: AlertStateSuppressed, InhibitedBy: ids}
}

func (m *SimpleMarker) SetSilenced(fp string, ids ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[fp] = AlertStatus{State: AlertStateSuppressed, SilencedBy: ids}
}

func (m *SimpleMarker) Status(fp string) AlertStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.statuses[fp]; ok {
		return s
	}
	return AlertStatus{State: AlertStateUnprocessed}
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Alertmanager API Metrics, Labels, Types 시뮬레이션 PoC     ║")
	fmt.Println("║  실제 소스: api/metrics/, pkg/labels/, types/               ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. API Alert Metrics ===
	fmt.Println("=== 1. API Alert Metrics ===")
	metrics := NewAlertMetrics()
	metrics.RecordFiring()
	metrics.RecordFiring()
	metrics.RecordFiring()
	metrics.RecordResolved()
	metrics.RecordInvalid()
	fmt.Println(metrics)
	fmt.Println()

	// === 2. HTTP Metrics ===
	fmt.Println("=== 2. HTTP Metrics ===")
	httpMetrics := NewHTTPMetrics()
	httpMetrics.RecordRequest("POST", "/api/v2/alerts", 200, 5*time.Millisecond)
	httpMetrics.RecordRequest("POST", "/api/v2/alerts", 200, 8*time.Millisecond)
	httpMetrics.RecordRequest("GET", "/api/v2/alerts", 200, 2*time.Millisecond)
	httpMetrics.RecordRequest("POST", "/api/v2/alerts", 400, 1*time.Millisecond)
	fmt.Println(httpMetrics)
	fmt.Println()

	// === 3. Matcher 생성 및 매칭 ===
	fmt.Println("=== 3. Matcher 매칭 ===")

	testLabels := LabelSet{
		"alertname": "HighCPU",
		"severity":  "critical",
		"instance":  "web-server-01:9090",
		"job":       "node-exporter",
		"env":       "production",
	}

	testMatchers := []struct {
		input string
		label LabelSet
	}{
		{`severity="critical"`, testLabels},
		{`severity!="warning"`, testLabels},
		{`instance=~"web-.*"`, testLabels},
		{`job!~".*exporter"`, testLabels},
		{`env="staging"`, testLabels},
	}

	for _, tc := range testMatchers {
		m, err := ParseMatcher(tc.input)
		if err != nil {
			fmt.Printf("  파싱 오류: %v\n", err)
			continue
		}
		result := m.Matches(tc.label[m.Name])
		fmt.Printf("  %s → %v (레이블 값=%q)\n", m, result, tc.label[m.Name])
	}
	fmt.Println()

	// === 4. 다중 Matcher 파싱 ===
	fmt.Println("=== 4. 다중 Matcher 파싱 ===")
	matcherStr := `{severity="critical", job=~"node.*", env!="staging"}`
	matchers, err := ParseMatchers(matcherStr)
	if err != nil {
		fmt.Printf("  파싱 오류: %v\n", err)
	} else {
		fmt.Printf("  입력: %s\n", matcherStr)
		for _, m := range matchers {
			fmt.Printf("    %s\n", m)
		}
		matched := MatchesAll(testLabels, matchers)
		fmt.Printf("  매칭 결과: %v\n", matched)
	}
	fmt.Println()

	// === 5. Alert 타입 ===
	fmt.Println("=== 5. Alert 타입 ===")
	firingAlert := &Alert{
		Labels:    LabelSet{"alertname": "HighCPU", "severity": "critical"},
		StartsAt:  time.Now().Add(-10 * time.Minute),
		GeneratorURL: "http://prometheus:9090/graph?g0.expr=cpu_usage%3E90",
	}
	resolvedAlert := &Alert{
		Labels:   LabelSet{"alertname": "DiskFull", "severity": "warning"},
		StartsAt: time.Now().Add(-30 * time.Minute),
		EndsAt:   time.Now().Add(-5 * time.Minute),
	}

	fmt.Printf("  HighCPU: status=%s, fingerprint=%s\n", firingAlert.Status(), firingAlert.Fingerprint())
	fmt.Printf("  DiskFull: status=%s, fingerprint=%s\n", resolvedAlert.Status(), resolvedAlert.Fingerprint())
	fmt.Println()

	// === 6. Marker (알림 상태 추적) ===
	fmt.Println("=== 6. Marker (알림 상태 추적) ===")
	marker := NewSimpleMarker()

	fp1 := firingAlert.Fingerprint()
	fp2 := resolvedAlert.Fingerprint()

	marker.SetActive(fp1)
	marker.SetSilenced(fp2, "silence-001", "silence-002")

	status1 := marker.Status(fp1)
	status2 := marker.Status(fp2)
	status3 := marker.Status("unknown-fp")

	fmt.Printf("  %s (HighCPU): state=%s\n", fp1, status1.State)
	fmt.Printf("  %s (DiskFull): state=%s, silencedBy=%v\n", fp2, status2.State, status2.SilencedBy)
	fmt.Printf("  unknown-fp: state=%s\n", status3.State)
	fmt.Println()

	// Inhibited 상태 설정
	marker.SetInhibited(fp1, "inhibit-rule-1")
	status1 = marker.Status(fp1)
	fmt.Printf("  %s 억제 후: state=%s, inhibitedBy=%v\n", fp1, status1.State, status1.InhibitedBy)
	fmt.Println()

	// === 7. LabelSet 필터링 ===
	fmt.Println("=== 7. LabelSet 필터링 ===")
	alerts := []struct {
		name   string
		labels LabelSet
	}{
		{"HighCPU", LabelSet{"alertname": "HighCPU", "severity": "critical", "env": "production"}},
		{"HighMemory", LabelSet{"alertname": "HighMemory", "severity": "warning", "env": "production"}},
		{"DiskFull", LabelSet{"alertname": "DiskFull", "severity": "critical", "env": "staging"}},
		{"NetworkError", LabelSet{"alertname": "NetworkError", "severity": "info", "env": "production"}},
	}

	filter, _ := ParseMatchers(`{severity="critical", env="production"}`)
	fmt.Printf("  필터: severity=critical AND env=production\n")
	for _, a := range alerts {
		matched := MatchesAll(a.labels, filter)
		if matched {
			fmt.Printf("    [일치] %s %v\n", a.name, a.labels)
		} else {
			fmt.Printf("    [제외] %s %v\n", a.name, a.labels)
		}
	}
}
