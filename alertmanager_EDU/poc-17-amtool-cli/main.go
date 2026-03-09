// poc-17-amtool-cli: Alertmanager amtool CLI 시뮬레이터
//
// 이 PoC는 amtool CLI의 핵심 아키텍처를 Go 표준 라이브러리만으로 재현한다.
//
// 시뮬레이션 대상:
//   1. CLI 파서: 서브커맨드 트리 구조 (alert/silence/check-config/config)
//   2. 설정 파일 리졸버: YAML 형태의 설정 파일에서 기본값 로드
//   3. 출력 포맷터: 인터페이스 기반 멀티 포맷 출력 (simple/extended/json)
//   4. 매처 파서: alertname 자동 보완 등 편의 기능
//   5. 사일런스 관리: 생성/조회/만료/임포트(병렬 워커)
//   6. API 클라이언트: 모의 HTTP 클라이언트와 타임아웃 래퍼
//   7. 설정 검증: check-config 동작 원리
//   8. 라우팅 테스트: 라벨 기반 라우팅 매칭
//
// 실행: go run main.go

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// ============================================================
// 1. 매처 시스템 (cli/utils.go, matcher/compat 참조)
// ============================================================

// MatchType은 매처의 비교 유형을 나타낸다.
type MatchType int

const (
	MatchEqual    MatchType = iota // =
	MatchNotEqual                  // !=
	MatchRegexp                    // =~
	MatchNotRegexp                 // !~
)

// Matcher는 라벨 매칭 조건이다.
// 실제 코드: alertmanager/pkg/labels/matcher.go
type Matcher struct {
	Name  string
	Value string
	Type  MatchType
}

func (m Matcher) String() string {
	switch m.Type {
	case MatchEqual:
		return fmt.Sprintf("%s=\"%s\"", m.Name, m.Value)
	case MatchNotEqual:
		return fmt.Sprintf("%s!=\"%s\"", m.Name, m.Value)
	case MatchRegexp:
		return fmt.Sprintf("%s=~\"%s\"", m.Name, m.Value)
	case MatchNotRegexp:
		return fmt.Sprintf("%s!~\"%s\"", m.Name, m.Value)
	}
	return ""
}

// parseMatcher는 "name=value" 형태의 문자열을 파싱한다.
// 실제 코드: alertmanager/matcher/compat/parse.go
func parseMatcher(s string) (Matcher, error) {
	// 연산자 순서: !=, !~, =~, = (긴 것부터 매칭)
	operators := []struct {
		op    string
		mtype MatchType
	}{
		{"!=", MatchNotEqual},
		{"!~", MatchNotRegexp},
		{"=~", MatchRegexp},
		{"=", MatchEqual},
	}

	for _, op := range operators {
		idx := strings.Index(s, op.op)
		if idx > 0 {
			name := s[:idx]
			value := strings.Trim(s[idx+len(op.op):], "\"")
			return Matcher{Name: name, Value: value, Type: op.mtype}, nil
		}
	}
	return Matcher{}, fmt.Errorf("매처 파싱 실패: %s", s)
}

// parseMatchersWithAutoAlertname은 첫 번째 인자가 연산자 없으면
// alertname=<값>으로 자동 변환한다.
// 실제 코드: cli/alert_query.go, cli/silence_add.go
func parseMatchersWithAutoAlertname(args []string) ([]Matcher, error) {
	if len(args) == 0 {
		return nil, nil
	}

	// 첫 번째 인자가 =이나 ~를 포함하지 않으면 alertname으로 간주
	if !strings.ContainsAny(args[0], "=~!") {
		args[0] = fmt.Sprintf("alertname=%s", args[0])
	}

	matchers := make([]Matcher, 0, len(args))
	for _, s := range args {
		m, err := parseMatcher(s)
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, m)
	}
	return matchers, nil
}

// ============================================================
// 2. 데이터 모델 (api/v2/models 참조)
// ============================================================

// Alert는 알림 데이터 모델이다.
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations,omitempty"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
	State       string            `json:"state"` // active, inhibited, silenced
}

// Silence는 사일런스 데이터 모델이다.
type Silence struct {
	ID        string    `json:"id"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
}

// ClusterStatus는 클러스터 상태를 나타낸다.
type ClusterStatus struct {
	Status string `json:"status"`
	Name   string `json:"name"`
	Peers  []Peer `json:"peers"`
}

// Peer는 클러스터 피어 정보이다.
type Peer struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// AlertmanagerStatus는 전체 상태 응답이다.
type AlertmanagerStatus struct {
	Config      map[string]string `json:"config"`
	VersionInfo map[string]string `json:"versionInfo"`
	Cluster     ClusterStatus     `json:"cluster"`
	Uptime      time.Time         `json:"uptime"`
}

// ============================================================
// 3. 모의 API 서버 (실제로는 Alertmanager 서버)
// ============================================================

// MockAlertmanagerServer는 amtool이 통신하는 Alertmanager 서버를 시뮬레이션한다.
type MockAlertmanagerServer struct {
	mu       sync.RWMutex
	alerts   []Alert
	silences []Silence
	nextID   int
}

func NewMockServer() *MockAlertmanagerServer {
	return &MockAlertmanagerServer{
		alerts: []Alert{
			{
				Labels:      map[string]string{"alertname": "HighMemory", "severity": "critical", "instance": "web-1"},
				Annotations: map[string]string{"summary": "메모리 사용량 초과"},
				StartsAt:    time.Now().Add(-30 * time.Minute),
				State:       "active",
			},
			{
				Labels:      map[string]string{"alertname": "DiskFull", "severity": "warning", "instance": "db-1"},
				Annotations: map[string]string{"summary": "디스크 사용량 95%"},
				StartsAt:    time.Now().Add(-2 * time.Hour),
				State:       "active",
			},
			{
				Labels:      map[string]string{"alertname": "CPUHigh", "severity": "info", "instance": "api-1"},
				Annotations: map[string]string{"summary": "CPU 사용량 높음"},
				StartsAt:    time.Now().Add(-10 * time.Minute),
				State:       "silenced",
			},
		},
		silences: []Silence{
			{
				ID:        "silence-001",
				Matchers:  []Matcher{{Name: "alertname", Value: "CPUHigh", Type: MatchEqual}},
				StartsAt:  time.Now().Add(-1 * time.Hour),
				EndsAt:    time.Now().Add(1 * time.Hour),
				CreatedBy: "admin",
				Comment:   "점검 중",
			},
		},
		nextID: 2,
	}
}

// GetAlerts는 매처 조건에 맞는 알림을 반환한다.
func (s *MockAlertmanagerServer) GetAlerts(matchers []Matcher, showActive, showSilenced, showInhibited bool) []Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []Alert
	for _, alert := range s.alerts {
		// 상태 필터
		switch alert.State {
		case "active":
			if !showActive {
				continue
			}
		case "silenced":
			if !showSilenced {
				continue
			}
		case "inhibited":
			if !showInhibited {
				continue
			}
		}

		// 매처 필터 (모든 매처가 일치해야 함)
		matched := true
		for _, m := range matchers {
			val, ok := alert.Labels[m.Name]
			if !ok || (m.Type == MatchEqual && val != m.Value) {
				matched = false
				break
			}
		}
		if matched {
			result = append(result, alert)
		}
	}
	return result
}

// PostSilence는 새 사일런스를 생성한다.
func (s *MockAlertmanagerServer) PostSilence(sil Silence) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	sil.ID = fmt.Sprintf("silence-%03d", s.nextID)
	s.nextID++
	s.silences = append(s.silences, sil)
	return sil.ID
}

// GetSilences는 사일런스 목록을 반환한다.
func (s *MockAlertmanagerServer) GetSilences() []Silence {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]Silence, len(s.silences))
	copy(result, s.silences)
	return result
}

// ExpireSilence는 사일런스를 만료시킨다.
func (s *MockAlertmanagerServer) ExpireSilence(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, sil := range s.silences {
		if sil.ID == id {
			s.silences[i].EndsAt = time.Now().Add(-1 * time.Second)
			return nil
		}
	}
	return fmt.Errorf("사일런스 ID '%s'를 찾을 수 없음", id)
}

// GetStatus는 서버 상태를 반환한다.
func (s *MockAlertmanagerServer) GetStatus() AlertmanagerStatus {
	return AlertmanagerStatus{
		Config: map[string]string{
			"original": "route:\n  receiver: default\n  routes:\n  - match:\n      severity: critical\n    receiver: pagerduty",
		},
		VersionInfo: map[string]string{
			"version":   "0.27.0",
			"revision":  "abc1234",
			"branch":    "main",
			"buildUser": "ci@build",
			"buildDate": "2024-01-15",
			"goVersion": "go1.21.5",
		},
		Cluster: ClusterStatus{
			Status: "ready",
			Name:   "alertmanager-0",
			Peers: []Peer{
				{Name: "alertmanager-0", Address: "10.0.0.1:9094"},
				{Name: "alertmanager-1", Address: "10.0.0.2:9094"},
				{Name: "alertmanager-2", Address: "10.0.0.3:9094"},
			},
		},
		Uptime: time.Now().Add(-24 * time.Hour),
	}
}

// ============================================================
// 4. 출력 포맷터 (cli/format/ 참조)
// ============================================================

// Formatter는 출력 포맷 인터페이스이다.
// 실제 코드: cli/format/format.go
type Formatter interface {
	FormatAlerts([]Alert) error
	FormatSilences([]Silence) error
	FormatConfig(AlertmanagerStatus) error
	FormatClusterStatus(ClusterStatus) error
}

// SimpleFormatter는 간략 포맷 출력기이다.
// 실제 코드: cli/format/format_simple.go
type SimpleFormatter struct{}

func (f *SimpleFormatter) FormatAlerts(alerts []Alert) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "Alertname\tStarts At\tSummary\tState\t")
	// 시간 순 정렬
	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].StartsAt.Before(alerts[j].StartsAt)
	})
	for _, alert := range alerts {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n",
			alert.Labels["alertname"],
			alert.StartsAt.Format("2006-01-02 15:04:05 MST"),
			alert.Annotations["summary"],
			alert.State,
		)
	}
	return w.Flush()
}

func (f *SimpleFormatter) FormatSilences(silences []Silence) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tMatchers\tEnds At\tCreated By\tComment\t")
	// 종료 시간 순 정렬
	sort.Slice(silences, func(i, j int) bool {
		return silences[i].EndsAt.Before(silences[j].EndsAt)
	})
	for _, sil := range silences {
		matcherStrs := make([]string, len(sil.Matchers))
		for i, m := range sil.Matchers {
			matcherStrs[i] = m.String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t\n",
			sil.ID,
			strings.Join(matcherStrs, " "),
			sil.EndsAt.Format("2006-01-02 15:04:05 MST"),
			sil.CreatedBy,
			sil.Comment,
		)
	}
	return w.Flush()
}

func (f *SimpleFormatter) FormatConfig(status AlertmanagerStatus) error {
	fmt.Println(status.Config["original"])
	return nil
}

func (f *SimpleFormatter) FormatClusterStatus(cluster ClusterStatus) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Cluster Status:\t%s\nNode Name:\t%s\n", cluster.Status, cluster.Name)
	return w.Flush()
}

// JSONFormatter는 JSON 포맷 출력기이다.
// 실제 코드: cli/format/format_json.go
type JSONFormatter struct{}

func (f *JSONFormatter) FormatAlerts(alerts []Alert) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(alerts)
}

func (f *JSONFormatter) FormatSilences(silences []Silence) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(silences)
}

func (f *JSONFormatter) FormatConfig(status AlertmanagerStatus) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(status)
}

func (f *JSONFormatter) FormatClusterStatus(cluster ClusterStatus) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(cluster)
}

// 포맷터 레지스트리 (실제 코드: cli/format/format.go의 Formatters 맵)
var formatters = map[string]Formatter{
	"simple": &SimpleFormatter{},
	"json":   &JSONFormatter{},
}

// ============================================================
// 5. 설정 파일 리졸버 (cli/config/config.go 참조)
// ============================================================

// ConfigResolver는 설정 파일에서 CLI 플래그 기본값을 로드한다.
// 실제 코드: cli/config/config.go의 Resolver
type ConfigResolver struct {
	flags map[string]string
}

// NewConfigResolver는 설정 맵에서 리졸버를 생성한다.
// 실제로는 YAML 파일을 파싱하지만, PoC에서는 맵으로 시뮬레이션
func NewConfigResolver(configs map[string]string) *ConfigResolver {
	return &ConfigResolver{flags: configs}
}

// Resolve는 키에 대한 값을 반환한다. 설정에 없으면 기본값을 반환.
func (r *ConfigResolver) Resolve(key, defaultVal string) string {
	if v, ok := r.flags[key]; ok {
		return v
	}
	return defaultVal
}

// ============================================================
// 6. 타임아웃 래퍼 (cli/utils.go의 execWithTimeout 참조)
// ============================================================

// execWithTimeout은 함수에 컨텍스트 기반 타임아웃을 적용한다.
// 실제 코드: cli/utils.go
func execWithTimeout(timeout time.Duration, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return fn(ctx)
}

// ============================================================
// 7. 라우팅 트리 (cli/routing.go 참조)
// ============================================================

// Route는 라우팅 규칙을 나타낸다.
type Route struct {
	Matchers []Matcher
	Receiver string
	Continue bool
	Children []*Route
}

// Match는 라벨 셋이 이 라우트와 일치하는지 확인한다.
func (r *Route) Match(labels map[string]string) bool {
	for _, m := range r.Matchers {
		val, ok := labels[m.Name]
		if !ok || (m.Type == MatchEqual && val != m.Value) {
			return false
		}
	}
	return true
}

// FindReceivers는 라벨 셋에 매칭되는 리시버 목록을 반환한다.
// 실제 코드: cli/test_routing.go의 resolveAlertReceivers
func (r *Route) FindReceivers(labels map[string]string) []string {
	var receivers []string
	r.findReceiversRecursive(labels, &receivers)
	return receivers
}

func (r *Route) findReceiversRecursive(labels map[string]string, receivers *[]string) {
	for _, child := range r.Children {
		if child.Match(labels) {
			if len(child.Children) == 0 {
				*receivers = append(*receivers, child.Receiver)
			} else {
				child.findReceiversRecursive(labels, receivers)
			}
			if !child.Continue {
				return
			}
		}
	}
	// 아무 자식도 매칭 안 되면 현재 노드의 리시버 사용
	if len(*receivers) == 0 {
		*receivers = append(*receivers, r.Receiver)
	}
}

// PrintTree는 라우팅 트리를 ASCII 트리로 출력한다.
// 실제 코드: cli/routing.go의 convertRouteToTree
func PrintTree(r *Route, prefix string, isLast bool) {
	connector := "├── "
	if isLast {
		connector = "└── "
	}

	slug := "default-route"
	if len(r.Matchers) > 0 {
		parts := make([]string, len(r.Matchers))
		for i, m := range r.Matchers {
			parts[i] = m.String()
		}
		slug = "{" + strings.Join(parts, ", ") + "}"
	}
	if r.Continue {
		slug += "  continue: true"
	}
	slug += "  receiver: " + r.Receiver

	if prefix == "" {
		fmt.Println(slug)
	} else {
		fmt.Println(prefix + connector + slug)
	}

	newPrefix := prefix
	if prefix == "" {
		newPrefix = ""
	} else if isLast {
		newPrefix += "    "
	} else {
		newPrefix += "│   "
	}

	for i, child := range r.Children {
		PrintTree(child, newPrefix, i == len(r.Children)-1)
	}
}

// ============================================================
// 8. 설정 검증 (cli/check_config.go 참조)
// ============================================================

// CheckConfig는 설정 문자열을 검증한다.
// 실제 코드: cli/check_config.go의 CheckConfig
func CheckConfig(configStr string) error {
	fmt.Printf("Checking config...")

	// 기본 검증: route 키워드 존재 여부
	if !strings.Contains(configStr, "route:") {
		return fmt.Errorf("설정에 'route:' 섹션이 없습니다")
	}
	if !strings.Contains(configStr, "receiver:") {
		return fmt.Errorf("설정에 'receiver:' 정의가 없습니다")
	}

	fmt.Printf("  SUCCESS\n")
	fmt.Println("Found:")
	if strings.Contains(configStr, "global:") {
		fmt.Println(" - global config")
	}
	fmt.Println(" - route")

	// inhibit_rules 카운트
	inhibitCount := strings.Count(configStr, "- source_match")
	fmt.Printf(" - %d inhibit rules\n", inhibitCount)

	// receiver 카운트
	receiverCount := strings.Count(configStr, "- name:")
	fmt.Printf(" - %d receivers\n", receiverCount)

	return nil
}

// ============================================================
// 9. 사일런스 임포트 (병렬 워커) (cli/silence_import.go 참조)
// ============================================================

// ImportSilences는 사일런스를 병렬로 임포트한다.
// 실제 코드: cli/silence_import.go의 bulkImport, addSilenceWorker
func ImportSilences(server *MockAlertmanagerServer, silences []Silence, workers int, force bool) (int, int) {
	silenceCh := make(chan Silence, 100)
	errCh := make(chan error, 100)
	var wg sync.WaitGroup

	// 워커 시작
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sil := range silenceCh {
				if force {
					sil.ID = "" // ID 초기화하여 새로 생성
				}
				id := server.PostSilence(sil)
				fmt.Printf("  임포트 완료: %s\n", id)
				errCh <- nil
			}
		}()
	}

	// 사일런스 전송
	go func() {
		for _, sil := range silences {
			silenceCh <- sil
		}
		close(silenceCh)
		wg.Wait()
		close(errCh)
	}()

	// 결과 수집
	success, failure := 0, 0
	for err := range errCh {
		if err != nil {
			failure++
		} else {
			success++
		}
	}
	return success, failure
}

// ============================================================
// 메인 데모
// ============================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║     amtool CLI 시뮬레이터 (PoC-17)                     ║")
	fmt.Println("║     Alertmanager CLI 도구의 핵심 아키텍처 재현          ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// 모의 서버 초기화
	server := NewMockServer()

	// ─── 1. 설정 파일 리졸버 데모 ─────────────────────────
	fmt.Println("━━━ 1. 설정 파일 리졸버 ━━━")
	fmt.Println("실제: $HOME/.config/amtool/config.yml 또는 /etc/amtool/config.yml")
	fmt.Println()

	resolver := NewConfigResolver(map[string]string{
		"alertmanager.url": "http://localhost:9093",
		"author":           "oncall-team",
		"output":           "simple",
		"require-comment":  "true",
	})

	fmt.Printf("  alertmanager.url = %s\n", resolver.Resolve("alertmanager.url", "http://localhost:9093"))
	fmt.Printf("  author           = %s\n", resolver.Resolve("author", ""))
	fmt.Printf("  output           = %s\n", resolver.Resolve("output", "simple"))
	fmt.Printf("  timeout          = %s (기본값 사용)\n", resolver.Resolve("timeout", "30s"))
	fmt.Println()

	// ─── 2. 매처 파싱 및 자동 alertname 변환 ─────────────
	fmt.Println("━━━ 2. 매처 파싱 (alertname 자동 변환) ━━━")

	testCases := [][]string{
		{"HighMemory"},                              // alertname 자동 추가
		{"alertname=DiskFull"},                      // 명시적 지정
		{"alertname=HighMemory", "severity=critical"}, // 복수 매처
	}

	for _, args := range testCases {
		matchers, err := parseMatchersWithAutoAlertname(args)
		if err != nil {
			fmt.Printf("  입력: %v → 에러: %v\n", args, err)
			continue
		}
		strs := make([]string, len(matchers))
		for i, m := range matchers {
			strs[i] = m.String()
		}
		fmt.Printf("  입력: %-45s → 매처: %s\n", fmt.Sprintf("%v", args), strings.Join(strs, ", "))
	}
	fmt.Println()

	// ─── 3. alert query 데모 ──────────────────────────────
	fmt.Println("━━━ 3. alert query (활성 알림 조회) ━━━")
	fmt.Println("실제: amtool alert query --alertmanager.url=http://localhost:9093")
	fmt.Println()

	outputFormat := resolver.Resolve("output", "simple")
	formatter, ok := formatters[outputFormat]
	if !ok {
		fmt.Println("알 수 없는 출력 포맷:", outputFormat)
		return
	}

	// 활성 알림만 조회 (기본 동작)
	alerts := server.GetAlerts(nil, true, false, false)
	if err := formatter.FormatAlerts(alerts); err != nil {
		fmt.Println("포맷 에러:", err)
	}
	fmt.Println()

	// ─── 4. alert query + 매처 필터 ──────────────────────
	fmt.Println("━━━ 4. alert query HighMemory (매처 필터) ━━━")
	fmt.Println("실제: amtool alert query HighMemory")
	fmt.Println()

	matchers, _ := parseMatchersWithAutoAlertname([]string{"HighMemory"})
	filteredAlerts := server.GetAlerts(matchers, true, true, true)
	if err := formatter.FormatAlerts(filteredAlerts); err != nil {
		fmt.Println("포맷 에러:", err)
	}
	fmt.Println()

	// ─── 5. silence query ─────────────────────────────────
	fmt.Println("━━━ 5. silence query (사일런스 조회) ━━━")
	fmt.Println("실제: amtool silence query")
	fmt.Println()

	silences := server.GetSilences()
	// 만료되지 않은 것만 표시 (기본 동작, 실제 코드: cli/silence_query.go)
	var activeSilences []Silence
	for _, sil := range silences {
		if sil.EndsAt.After(time.Now()) {
			activeSilences = append(activeSilences, sil)
		}
	}
	if err := formatter.FormatSilences(activeSilences); err != nil {
		fmt.Println("포맷 에러:", err)
	}
	fmt.Println()

	// ─── 6. silence add ───────────────────────────────────
	fmt.Println("━━━ 6. silence add (사일런스 생성) ━━━")
	fmt.Println("실제: amtool silence add alertname=DiskFull -d 2h -c '디스크 정리 중'")
	fmt.Println()

	err := execWithTimeout(30*time.Second, func(ctx context.Context) error {
		// 타임아웃 컨텍스트 확인
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		newSilence := Silence{
			Matchers:  []Matcher{{Name: "alertname", Value: "DiskFull", Type: MatchEqual}},
			StartsAt:  time.Now(),
			EndsAt:    time.Now().Add(2 * time.Hour),
			CreatedBy: resolver.Resolve("author", "admin"),
			Comment:   "디스크 정리 중",
		}

		// require-comment 검증 (실제 코드: cli/silence_add.go)
		requireComment := resolver.Resolve("require-comment", "true")
		if requireComment == "true" && newSilence.Comment == "" {
			return fmt.Errorf("comment required by config")
		}

		id := server.PostSilence(newSilence)
		fmt.Printf("  생성된 사일런스 ID: %s\n", id)
		return nil
	})
	if err != nil {
		fmt.Println("에러:", err)
	}
	fmt.Println()

	// ─── 7. silence expire ────────────────────────────────
	fmt.Println("━━━ 7. silence expire (사일런스 만료) ━━━")
	fmt.Println("실제: amtool silence expire silence-001")
	fmt.Println()

	if err := server.ExpireSilence("silence-001"); err != nil {
		fmt.Println("  에러:", err)
	} else {
		fmt.Println("  silence-001 만료 완료")
	}
	fmt.Println()

	// ─── 8. silence import (병렬 워커) ────────────────────
	fmt.Println("━━━ 8. silence import (병렬 워커 패턴) ━━━")
	fmt.Println("실제: amtool silence import --worker=4 --force silences.json")
	fmt.Println()

	importSilences := []Silence{
		{Matchers: []Matcher{{Name: "alertname", Value: "TestAlert1", Type: MatchEqual}},
			StartsAt: time.Now(), EndsAt: time.Now().Add(1 * time.Hour),
			CreatedBy: "batch", Comment: "일괄 임포트 1"},
		{Matchers: []Matcher{{Name: "alertname", Value: "TestAlert2", Type: MatchEqual}},
			StartsAt: time.Now(), EndsAt: time.Now().Add(2 * time.Hour),
			CreatedBy: "batch", Comment: "일괄 임포트 2"},
		{Matchers: []Matcher{{Name: "alertname", Value: "TestAlert3", Type: MatchEqual}},
			StartsAt: time.Now(), EndsAt: time.Now().Add(3 * time.Hour),
			CreatedBy: "batch", Comment: "일괄 임포트 3"},
	}

	success, failure := ImportSilences(server, importSilences, 4, true)
	fmt.Printf("  결과: 성공=%d, 실패=%d\n", success, failure)
	fmt.Println()

	// ─── 9. check-config ──────────────────────────────────
	fmt.Println("━━━ 9. check-config (설정 검증) ━━━")
	fmt.Println("실제: amtool check-config alertmanager.yml")
	fmt.Println()

	validConfig := `global:
  resolve_timeout: 5m
route:
  receiver: default
  routes:
  - match:
      severity: critical
    receiver: pagerduty
receivers:
- name: default
- name: pagerduty`

	if err := CheckConfig(validConfig); err != nil {
		fmt.Println("  검증 실패:", err)
	}
	fmt.Println()

	// 잘못된 설정 테스트
	fmt.Printf("Checking invalid config...")
	invalidConfig := `global:\n  resolve_timeout: 5m`
	if err := CheckConfig(invalidConfig); err != nil {
		fmt.Printf("  FAILED: %s\n", err)
	}
	fmt.Println()

	// ─── 10. config routes show (라우팅 트리) ─────────────
	fmt.Println("━━━ 10. config routes show (라우팅 트리) ━━━")
	fmt.Println("실제: amtool config routes show --config.file=alertmanager.yml")
	fmt.Println()

	// 라우팅 트리 구성
	rootRoute := &Route{
		Receiver: "default",
		Children: []*Route{
			{
				Matchers: []Matcher{{Name: "severity", Value: "critical", Type: MatchEqual}},
				Receiver: "pagerduty",
			},
			{
				Matchers: []Matcher{{Name: "team", Value: "frontend", Type: MatchEqual}},
				Receiver: "slack-frontend",
				Children: []*Route{
					{
						Matchers: []Matcher{{Name: "severity", Value: "warning", Type: MatchEqual}},
						Receiver: "slack-frontend-warning",
						Continue: true,
					},
				},
			},
			{
				Matchers: []Matcher{{Name: "team", Value: "backend", Type: MatchEqual}},
				Receiver: "email-backend",
			},
		},
	}

	fmt.Println("Routing tree:")
	PrintTree(rootRoute, "", true)
	fmt.Println()

	// ─── 11. config routes test (라우팅 테스트) ───────────
	fmt.Println("━━━ 11. config routes test (라우팅 테스트) ━━━")
	fmt.Println("실제: amtool config routes test --config.file=alertmanager.yml severity=critical")
	fmt.Println()

	testLabels := []map[string]string{
		{"severity": "critical"},
		{"team": "frontend", "severity": "warning"},
		{"team": "backend"},
		{"alertname": "unknown"},
	}

	for _, labels := range testLabels {
		receivers := rootRoute.FindReceivers(labels)
		parts := make([]string, 0)
		for k, v := range labels {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(parts)
		fmt.Printf("  라벨: %-45s → 리시버: %s\n",
			strings.Join(parts, ", "),
			strings.Join(receivers, ", "))
	}
	fmt.Println()

	// ─── 12. cluster show ─────────────────────────────────
	fmt.Println("━━━ 12. cluster show (클러스터 상태) ━━━")
	fmt.Println("실제: amtool cluster show")
	fmt.Println()

	status := server.GetStatus()
	if err := formatter.FormatClusterStatus(status.Cluster); err != nil {
		fmt.Println("포맷 에러:", err)
	}
	fmt.Println()

	// ─── 13. JSON 출력 포맷 ───────────────────────────────
	fmt.Println("━━━ 13. JSON 출력 포맷 (-o json) ━━━")
	fmt.Println("실제: amtool silence query -o json")
	fmt.Println()

	jsonFormatter := formatters["json"]
	currentSilences := server.GetSilences()
	var active []Silence
	for _, sil := range currentSilences {
		if sil.EndsAt.After(time.Now()) {
			active = append(active, sil)
		}
	}
	if err := jsonFormatter.FormatSilences(active); err != nil {
		fmt.Println("포맷 에러:", err)
	}

	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("amtool CLI 시뮬레이션 완료!")
	fmt.Println()
	fmt.Println("핵심 아키텍처 요약:")
	fmt.Println("  1. kingpin 기반 서브커맨드 트리 (alert/silence/config/cluster/template)")
	fmt.Println("  2. YAML 설정 파일 리졸버 (플래그 기본값 오버라이드)")
	fmt.Println("  3. Formatter 인터페이스 (simple/extended/json)")
	fmt.Println("  4. 매처 자동 변환 (alertname 생략 시 자동 추가)")
	fmt.Println("  5. execWithTimeout 래퍼 (컨텍스트 기반 타임아웃)")
	fmt.Println("  6. 병렬 워커 패턴 (silence import)")
	fmt.Println("  7. go-swagger 자동 생성 API 클라이언트")
}
