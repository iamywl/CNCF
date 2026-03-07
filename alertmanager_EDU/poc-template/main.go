// Alertmanager Template Engine PoC
//
// Alertmanager의 템플릿 엔진을 시뮬레이션한다.
// template/template.go의 Data 구조체와 렌더링 함수를 재현한다.
//
// 핵심 개념:
//   - Data 구조체: Receiver, Status, Alerts, GroupLabels 등
//   - Go text/template 기반 렌더링
//   - 커스텀 함수 (toUpper, title, join 등)
//   - Slack/Email용 기본 템플릿
//
// 실행: go run main.go

package main

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"
)

// KV는 키-값 쌍 맵이다.
type KV map[string]string

// SortedPairs는 KV를 정렬된 Pair 슬라이스로 변환한다.
func (kv KV) SortedPairs() []Pair {
	pairs := make([]Pair, 0, len(kv))
	for k, v := range kv {
		pairs = append(pairs, Pair{Name: k, Value: v})
	}
	return pairs
}

// Remove는 지정된 키를 제외한 새 KV를 반환한다.
func (kv KV) Remove(keys []string) KV {
	result := make(KV)
	exclude := make(map[string]bool)
	for _, k := range keys {
		exclude[k] = true
	}
	for k, v := range kv {
		if !exclude[k] {
			result[k] = v
		}
	}
	return result
}

// Pair는 정렬된 키-값 쌍이다.
type Pair struct {
	Name  string
	Value string
}

// Alert는 템플릿용 Alert 데이터이다.
type Alert struct {
	Status      string
	Labels      KV
	Annotations KV
	StartsAt    time.Time
	EndsAt      time.Time
	Fingerprint string
}

// Alerts는 Alert 슬라이스이다.
type Alerts []Alert

// Firing은 firing 상태의 Alert만 반환한다.
func (as Alerts) Firing() Alerts {
	var result Alerts
	for _, a := range as {
		if a.Status == "firing" {
			result = append(result, a)
		}
	}
	return result
}

// Resolved는 resolved 상태의 Alert만 반환한다.
func (as Alerts) Resolved() Alerts {
	var result Alerts
	for _, a := range as {
		if a.Status == "resolved" {
			result = append(result, a)
		}
	}
	return result
}

// Data는 템플릿에 전달되는 최상위 데이터이다.
type Data struct {
	Receiver          string
	Status            string
	Alerts            Alerts
	GroupLabels       KV
	CommonLabels      KV
	CommonAnnotations KV
	ExternalURL       string
}

// 커스텀 템플릿 함수
var defaultFuncs = template.FuncMap{
	"toUpper": strings.ToUpper,
	"toLower": strings.ToLower,
	"title":   strings.Title,
	"join":    strings.Join,
	"safeHtml": func(s string) string {
		return s // 간소화: HTML 이스케이프 없음
	},
	"stringSlice": func(args ...string) []string {
		return args
	},
}

// 기본 Slack 템플릿
const slackTemplate = `{{ define "slack.default.title" -}}
[{{ .Status | toUpper }}{{ if eq .Status "firing" }}:{{ .Alerts.Firing | len }}{{ end }}] {{ .GroupLabels.SortedPairs | formatPairs }}
{{- end }}

{{ define "slack.default.text" -}}
{{ range .Alerts }}
*Alert:* {{ .Labels.alertname }} - {{ .Labels.severity }}
*Description:* {{ .Annotations.description }}
*Started:* {{ .StartsAt.Format "2006-01-02 15:04:05" }}
{{ if eq .Status "resolved" }}*Resolved:* {{ .EndsAt.Format "2006-01-02 15:04:05" }}{{ end }}
{{ end }}
{{- end }}`

// Email 템플릿
const emailTemplate = `{{ define "email.default.subject" -}}
[{{ .Status | toUpper }}] {{ .GroupLabels.alertname }}: {{ .Alerts | len }}개 Alert
{{- end }}

{{ define "email.default.body" -}}
수신자: {{ .Receiver }}
상태: {{ .Status | toUpper }}
그룹: {{ range $k, $v := .GroupLabels }}{{ $k }}={{ $v }} {{ end }}

=== Firing Alerts ({{ .Alerts.Firing | len }}개) ===
{{ range .Alerts.Firing -}}
  Alert: {{ .Labels.alertname }}
  Severity: {{ .Labels.severity }}
  Instance: {{ .Labels.instance }}
  설명: {{ .Annotations.description }}
  시작: {{ .StartsAt.Format "2006-01-02 15:04:05" }}
  ---
{{ end }}
{{ if .Alerts.Resolved -}}
=== Resolved Alerts ({{ .Alerts.Resolved | len }}개) ===
{{ range .Alerts.Resolved -}}
  Alert: {{ .Labels.alertname }}
  해결: {{ .EndsAt.Format "2006-01-02 15:04:05" }}
  ---
{{ end }}
{{- end }}
{{- end }}`

// formatPairs는 Pair 슬라이스를 문자열로 포맷한다.
func formatPairs(pairs []Pair) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.Name + "=" + p.Value
	}
	return strings.Join(parts, " ")
}

// renderTemplate은 템플릿을 렌더링한다.
func renderTemplate(tmplStr, name string, data *Data) (string, error) {
	funcs := template.FuncMap{}
	for k, v := range defaultFuncs {
		funcs[k] = v
	}
	funcs["formatPairs"] = formatPairs

	tmpl, err := template.New("root").Funcs(funcs).Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("템플릿 파싱 오류: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("템플릿 실행 오류: %w", err)
	}

	return buf.String(), nil
}

func main() {
	fmt.Println("=== Alertmanager Template Engine PoC ===")
	fmt.Println()

	now := time.Now()

	// 템플릿 데이터 생성
	data := &Data{
		Receiver: "slack-infra",
		Status:   "firing",
		Alerts: Alerts{
			{
				Status:      "firing",
				Labels:      KV{"alertname": "HighCPU", "severity": "critical", "instance": "node-1"},
				Annotations: KV{"description": "CPU usage above 90% for 5 minutes", "runbook": "https://wiki/cpu"},
				StartsAt:    now.Add(-10 * time.Minute),
				Fingerprint: "abc123",
			},
			{
				Status:      "firing",
				Labels:      KV{"alertname": "HighCPU", "severity": "warning", "instance": "node-2"},
				Annotations: KV{"description": "CPU usage above 80% for 10 minutes"},
				StartsAt:    now.Add(-5 * time.Minute),
				Fingerprint: "def456",
			},
			{
				Status:      "resolved",
				Labels:      KV{"alertname": "HighCPU", "severity": "critical", "instance": "node-3"},
				Annotations: KV{"description": "CPU usage normalized"},
				StartsAt:    now.Add(-30 * time.Minute),
				EndsAt:      now.Add(-2 * time.Minute),
				Fingerprint: "ghi789",
			},
		},
		GroupLabels:       KV{"alertname": "HighCPU"},
		CommonLabels:      KV{"alertname": "HighCPU", "job": "node-exporter"},
		CommonAnnotations: KV{},
		ExternalURL:       "http://alertmanager:9093",
	}

	// 1. Alerts 필터링
	fmt.Println("--- 1. Alert 필터링 ---")
	fmt.Printf("전체 Alert: %d개\n", len(data.Alerts))
	fmt.Printf("Firing: %d개\n", len(data.Alerts.Firing()))
	fmt.Printf("Resolved: %d개\n", len(data.Alerts.Resolved()))
	fmt.Println()

	// 2. KV 메서드
	fmt.Println("--- 2. KV 메서드 ---")
	fmt.Printf("GroupLabels: %v\n", data.GroupLabels)
	fmt.Printf("SortedPairs: %v\n", data.GroupLabels.SortedPairs())

	commonWithout := data.CommonLabels.Remove([]string{"alertname"})
	fmt.Printf("CommonLabels (alertname 제외): %v\n", commonWithout)
	fmt.Println()

	// 3. Email 템플릿 렌더링
	fmt.Println("--- 3. Email 템플릿 렌더링 ---")
	subject, err := renderTemplate(emailTemplate, "email.default.subject", data)
	if err != nil {
		fmt.Printf("오류: %v\n", err)
	} else {
		fmt.Printf("Subject: %s\n", strings.TrimSpace(subject))
	}

	body, err := renderTemplate(emailTemplate, "email.default.body", data)
	if err != nil {
		fmt.Printf("오류: %v\n", err)
	} else {
		fmt.Println("Body:")
		fmt.Println(body)
	}

	// 4. 커스텀 템플릿
	fmt.Println("--- 4. 커스텀 템플릿 ---")
	customTmpl := `{{ define "custom" -}}
🔔 {{ .Status | toUpper }} | {{ .Receiver }}
{{ range .Alerts.Firing -}}
  ⚠️ {{ .Labels.alertname }} ({{ .Labels.severity }}) on {{ .Labels.instance }}
{{ end -}}
{{ range .Alerts.Resolved -}}
  ✅ {{ .Labels.alertname }} resolved on {{ .Labels.instance }}
{{ end -}}
{{- end }}`

	custom, err := renderTemplate(customTmpl, "custom", data)
	if err != nil {
		fmt.Printf("오류: %v\n", err)
	} else {
		fmt.Println(custom)
	}

	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. Data 구조체로 Receiver, Status, Alerts, GroupLabels 전달")
	fmt.Println("2. Alerts.Firing(), Alerts.Resolved()로 상태별 필터링")
	fmt.Println("3. KV.SortedPairs(), KV.Remove()로 레이블 조작")
	fmt.Println("4. Go text/template + 커스텀 함수로 렌더링")
	fmt.Println("5. define/ExecuteTemplate로 여러 템플릿 정의/호출")
}
