// Package main은 Alertmanager의 Receiver(알림 수신자) 구현체를
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Notifier 인터페이스 (Notify 메서드 패턴)
// 2. 공통 생성자 패턴 (HTTP 클라이언트 + 템플릿 + 로거)
// 3. Retrier 패턴 (HTTP 상태코드별 재시도 판단)
// 4. 템플릿 기반 메시지 생성
// 5. Slack Receiver (Block Kit JSON)
// 6. Webhook Receiver (JSON POST)
// 7. Email Receiver (SMTP 시뮬레이션)
// 8. PagerDuty Receiver (이벤트 API)
// 9. 알림 그룹핑 (GroupKey 기반)
// 10. 수신자 팩토리 패턴
//
// 실제 소스 참조:
//   - notify/notify.go              (Notifier 인터페이스)
//   - notify/slack/slack.go         (Slack Receiver)
//   - notify/webhook/webhook.go     (Webhook Receiver)
//   - notify/email/email.go         (Email Receiver)
//   - notify/pagerduty/pagerduty.go (PagerDuty Receiver)
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"
)

// ============================================================================
// 1. 핵심 타입 (types/types.go 시뮬레이션)
// ============================================================================

// Alert는 단일 알림이다.
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt,omitempty"`
	Status      string            `json:"status"` // "firing" or "resolved"
}

// GroupKey는 알림 그룹을 식별하는 키다.
type GroupKey string

// ============================================================================
// 2. Notifier 인터페이스 (notify/notify.go 시뮬레이션)
// ============================================================================

// Notifier는 알림을 전송하는 인터페이스다.
// 반환값: retry (재시도 가능 여부), error
type Notifier interface {
	Notify(alerts ...*Alert) (bool, error)
}

// ============================================================================
// 3. Retrier (notify/notify.go 시뮬레이션)
// ============================================================================

// Retrier는 HTTP 응답 코드를 분석하여 재시도 여부를 결정한다.
type Retrier struct {
	RetryCodes []int
}

// Check는 HTTP 상태코드가 재시도 대상인지 확인한다.
func (r *Retrier) Check(statusCode int) (bool, error) {
	// 2xx: 성공
	if statusCode >= 200 && statusCode < 300 {
		return false, nil
	}

	// 재시도 코드 목록 확인
	for _, code := range r.RetryCodes {
		if statusCode == code {
			return true, fmt.Errorf("HTTP %d: 일시적 오류 (재시도 가능)", statusCode)
		}
	}

	// 나머지: 영구적 실패
	return false, fmt.Errorf("HTTP %d: 영구적 오류", statusCode)
}

// ============================================================================
// 4. 템플릿 시스템 (template/template.go 시뮬레이션)
// ============================================================================

// TemplateData는 템플릿 렌더링에 사용하는 데이터다.
type TemplateData struct {
	Receiver       string
	Status         string
	GroupLabels    map[string]string
	CommonLabels   map[string]string
	Alerts         []*Alert
	FiringAlerts   []*Alert
	ResolvedAlerts []*Alert
}

// PrepareTemplateData는 알림 목록에서 템플릿 데이터를 준비한다.
func PrepareTemplateData(receiver string, groupLabels map[string]string, alerts ...*Alert) TemplateData {
	data := TemplateData{
		Receiver:    receiver,
		GroupLabels: groupLabels,
		Alerts:      alerts,
	}

	// 공통 레이블 추출
	if len(alerts) > 0 {
		common := make(map[string]string)
		for k, v := range alerts[0].Labels {
			common[k] = v
		}
		for _, a := range alerts[1:] {
			for k, v := range common {
				if a.Labels[k] != v {
					delete(common, k)
				}
			}
		}
		data.CommonLabels = common
	}

	// Firing/Resolved 분류
	for _, a := range alerts {
		if a.Status == "firing" {
			data.FiringAlerts = append(data.FiringAlerts, a)
			data.Status = "firing"
		} else {
			data.ResolvedAlerts = append(data.ResolvedAlerts, a)
		}
	}
	if len(data.FiringAlerts) == 0 {
		data.Status = "resolved"
	}

	return data
}

// RenderTemplate은 Go 템플릿으로 메시지를 렌더링한다.
func RenderTemplate(tmplStr string, data TemplateData) (string, error) {
	funcMap := template.FuncMap{
		"toUpper": strings.ToUpper,
		"join":    strings.Join,
	}
	tmpl, err := template.New("msg").Funcs(funcMap).Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ============================================================================
// 5. Slack Receiver (notify/slack/slack.go 시뮬레이션)
// ============================================================================

// SlackNotifier는 Slack으로 알림을 전송한다.
type SlackNotifier struct {
	WebhookURL string
	Channel    string
	Username   string
	IconEmoji  string
	retrier    *Retrier
}

// NewSlackNotifier는 새 Slack Receiver를 생성한다.
func NewSlackNotifier(webhookURL, channel string) *SlackNotifier {
	return &SlackNotifier{
		WebhookURL: webhookURL,
		Channel:    channel,
		Username:   "Alertmanager",
		IconEmoji:  ":warning:",
		retrier:    &Retrier{RetryCodes: []int{429, 500, 502, 503, 504}},
	}
}

// Notify는 Slack으로 알림을 전송한다.
func (s *SlackNotifier) Notify(alerts ...*Alert) (bool, error) {
	data := PrepareTemplateData("slack", map[string]string{}, alerts...)

	// Slack Block Kit JSON 생성
	payload := map[string]interface{}{
		"channel":  s.Channel,
		"username": s.Username,
		"blocks": []map[string]interface{}{
			{
				"type": "header",
				"text": map[string]string{
					"type": "plain_text",
					"text": fmt.Sprintf("[%s] %d Alerts", strings.ToUpper(data.Status), len(alerts)),
				},
			},
		},
	}

	for _, a := range alerts {
		block := map[string]interface{}{
			"type": "section",
			"text": map[string]string{
				"type": "mrkdwn",
				"text": fmt.Sprintf("*%s* - %s\n%s",
					a.Labels["alertname"],
					a.Status,
					a.Annotations["description"]),
			},
		}
		payload["blocks"] = append(payload["blocks"].([]map[string]interface{}), block)
	}

	payloadJSON, _ := json.MarshalIndent(payload, "    ", "  ")
	fmt.Printf("    [Slack] POST %s\n    %s\n", s.WebhookURL, string(payloadJSON))

	// HTTP 응답 시뮬레이션 (200 OK)
	return s.retrier.Check(200)
}

// ============================================================================
// 6. Webhook Receiver (notify/webhook/webhook.go 시뮬레이션)
// ============================================================================

// WebhookNotifier는 일반 HTTP 웹훅으로 알림을 전송한다.
type WebhookNotifier struct {
	URL     string
	retrier *Retrier
}

// NewWebhookNotifier는 새 Webhook Receiver를 생성한다.
func NewWebhookNotifier(url string) *WebhookNotifier {
	return &WebhookNotifier{
		URL:     url,
		retrier: &Retrier{RetryCodes: []int{429, 500, 502, 503, 504}},
	}
}

// Notify는 웹훅으로 알림을 전송한다.
func (w *WebhookNotifier) Notify(alerts ...*Alert) (bool, error) {
	data := PrepareTemplateData("webhook", map[string]string{}, alerts...)

	payload := map[string]interface{}{
		"receiver": data.Receiver,
		"status":   data.Status,
		"alerts":   alerts,
	}

	payloadJSON, _ := json.MarshalIndent(payload, "    ", "  ")
	fmt.Printf("    [Webhook] POST %s\n    %s\n", w.URL, string(payloadJSON))
	return w.retrier.Check(200)
}

// ============================================================================
// 7. Email Receiver (notify/email/email.go 시뮬레이션)
// ============================================================================

// EmailNotifier는 이메일로 알림을 전송한다.
type EmailNotifier struct {
	To       string
	From     string
	SMTPHost string
	Subject  string
}

// NewEmailNotifier는 새 Email Receiver를 생성한다.
func NewEmailNotifier(to, from, smtpHost string) *EmailNotifier {
	return &EmailNotifier{
		To:       to,
		From:     from,
		SMTPHost: smtpHost,
		Subject:  "[{{ .Status | toUpper }}] {{ .GroupLabels.alertname }}",
	}
}

// Notify는 이메일로 알림을 전송한다.
func (e *EmailNotifier) Notify(alerts ...*Alert) (bool, error) {
	data := PrepareTemplateData("email", map[string]string{
		"alertname": alerts[0].Labels["alertname"],
	}, alerts...)

	subject, _ := RenderTemplate(e.Subject, data)

	fmt.Printf("    [Email] SMTP %s\n", e.SMTPHost)
	fmt.Printf("    From: %s\n", e.From)
	fmt.Printf("    To: %s\n", e.To)
	fmt.Printf("    Subject: %s\n", subject)
	fmt.Printf("    Body: %d개 알림 (%d firing, %d resolved)\n",
		len(alerts), len(data.FiringAlerts), len(data.ResolvedAlerts))

	return false, nil // 이메일은 재시도 불가
}

// ============================================================================
// 8. PagerDuty Receiver (notify/pagerduty/pagerduty.go 시뮬레이션)
// ============================================================================

// PagerDutyNotifier는 PagerDuty로 알림을 전송한다.
type PagerDutyNotifier struct {
	RoutingKey string
	retrier    *Retrier
}

// NewPagerDutyNotifier는 새 PagerDuty Receiver를 생성한다.
func NewPagerDutyNotifier(routingKey string) *PagerDutyNotifier {
	return &PagerDutyNotifier{
		RoutingKey: routingKey,
		retrier:    &Retrier{RetryCodes: []int{429, 500, 502, 503, 504}},
	}
}

// Notify는 PagerDuty Events API v2로 알림을 전송한다.
func (p *PagerDutyNotifier) Notify(alerts ...*Alert) (bool, error) {
	for _, a := range alerts {
		eventAction := "trigger"
		if a.Status == "resolved" {
			eventAction = "resolve"
		}

		severity := "warning"
		if sev, ok := a.Labels["severity"]; ok {
			severity = sev
		}

		payload := map[string]interface{}{
			"routing_key":  p.RoutingKey,
			"event_action": eventAction,
			"dedup_key":    a.Labels["alertname"] + "/" + a.Labels["instance"],
			"payload": map[string]interface{}{
				"summary":  a.Annotations["summary"],
				"severity": severity,
				"source":   a.Labels["instance"],
			},
		}

		payloadJSON, _ := json.MarshalIndent(payload, "    ", "  ")
		fmt.Printf("    [PagerDuty] POST https://events.pagerduty.com/v2/enqueue\n")
		fmt.Printf("    %s\n", string(payloadJSON))
	}

	return p.retrier.Check(202) // PagerDuty는 202 Accepted로 응답
}

// ============================================================================
// 9. 수신자 팩토리 (notify/notify.go 시뮬레이션)
// ============================================================================

// ReceiverFactory는 설정 기반으로 Notifier를 생성한다.
func ReceiverFactory(receiverType string, config map[string]string) Notifier {
	switch receiverType {
	case "slack":
		return NewSlackNotifier(config["webhook_url"], config["channel"])
	case "webhook":
		return NewWebhookNotifier(config["url"])
	case "email":
		return NewEmailNotifier(config["to"], config["from"], config["smtp_host"])
	case "pagerduty":
		return NewPagerDutyNotifier(config["routing_key"])
	default:
		return nil
	}
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Alertmanager Receiver 구현체 시뮬레이션 PoC                ║")
	fmt.Println("║  실제 소스: notify/slack/, webhook/, email/, pagerduty/     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// 테스트 알림 생성
	alerts := []*Alert{
		{
			Labels: map[string]string{
				"alertname": "HighCPU",
				"severity":  "critical",
				"instance":  "web-server-01:9090",
				"job":       "node",
			},
			Annotations: map[string]string{
				"summary":     "CPU 사용률 95% 초과",
				"description": "web-server-01의 CPU 사용률이 95%를 초과했습니다.",
			},
			StartsAt: time.Now().Add(-5 * time.Minute),
			Status:   "firing",
		},
		{
			Labels: map[string]string{
				"alertname": "HighMemory",
				"severity":  "warning",
				"instance":  "web-server-02:9090",
				"job":       "node",
			},
			Annotations: map[string]string{
				"summary":     "메모리 사용률 85% 초과",
				"description": "web-server-02의 메모리 사용률이 85%를 초과했습니다.",
			},
			StartsAt: time.Now().Add(-2 * time.Minute),
			Status:   "firing",
		},
		{
			Labels: map[string]string{
				"alertname": "DiskFull",
				"severity":  "warning",
				"instance":  "db-server-01:9090",
				"job":       "node",
			},
			Annotations: map[string]string{
				"summary":     "디스크 사용률 정상 복구",
				"description": "db-server-01의 디스크 사용률이 정상 범위로 돌아왔습니다.",
			},
			StartsAt: time.Now().Add(-30 * time.Minute),
			EndsAt:   time.Now().Add(-1 * time.Minute),
			Status:   "resolved",
		},
	}

	// === 1. Slack Receiver ===
	fmt.Println("=== 1. Slack Receiver ===")
	slack := NewSlackNotifier("https://hooks.slack.com/services/T00/B00/xxx", "#alerts")
	retry, err := slack.Notify(alerts...)
	fmt.Printf("    결과: retry=%v, err=%v\n\n", retry, err)

	// === 2. Webhook Receiver ===
	fmt.Println("=== 2. Webhook Receiver ===")
	webhook := NewWebhookNotifier("https://alerts.example.com/webhook")
	retry, err = webhook.Notify(alerts[:1]...) // 첫 번째 알림만
	fmt.Printf("    결과: retry=%v, err=%v\n\n", retry, err)

	// === 3. Email Receiver ===
	fmt.Println("=== 3. Email Receiver ===")
	email := NewEmailNotifier("ops@example.com", "alertmanager@example.com", "smtp.example.com:587")
	retry, err = email.Notify(alerts...)
	fmt.Printf("    결과: retry=%v, err=%v\n\n", retry, err)

	// === 4. PagerDuty Receiver ===
	fmt.Println("=== 4. PagerDuty Receiver ===")
	pd := NewPagerDutyNotifier("R01234567890ABCDEF")
	retry, err = pd.Notify(alerts[:2]...)
	fmt.Printf("    결과: retry=%v, err=%v\n\n", retry, err)

	// === 5. Retrier 패턴 ===
	fmt.Println("=== 5. Retrier 패턴 데모 ===")
	retrier := &Retrier{RetryCodes: []int{429, 500, 502, 503, 504}}
	testCodes := []int{200, 201, 400, 404, 429, 500, 502, 503}
	for _, code := range testCodes {
		shouldRetry, retryErr := retrier.Check(code)
		status := "성공"
		if retryErr != nil {
			if shouldRetry {
				status = "재시도"
			} else {
				status = "영구 실패"
			}
		}
		fmt.Printf("    HTTP %d: %s (retry=%v)\n", code, status, shouldRetry)
	}
	fmt.Println()

	// === 6. 팩토리 패턴 ===
	fmt.Println("=== 6. 수신자 팩토리 패턴 ===")
	receivers := map[string]map[string]string{
		"slack":     {"webhook_url": "https://hooks.slack.com/xxx", "channel": "#ops"},
		"webhook":   {"url": "https://alerts.example.com/hook"},
		"pagerduty": {"routing_key": "R0123456789"},
	}
	for name, cfg := range receivers {
		notifier := ReceiverFactory(name, cfg)
		if notifier != nil {
			fmt.Printf("    %s: 생성 성공 (%T)\n", name, notifier)
		}
	}
	fmt.Println()

	// === 7. 템플릿 데이터 준비 ===
	fmt.Println("=== 7. 템플릿 데이터 준비 ===")
	data := PrepareTemplateData("slack", map[string]string{"alertname": "HighCPU"}, alerts...)
	fmt.Printf("    Receiver: %s\n", data.Receiver)
	fmt.Printf("    Status: %s\n", data.Status)
	fmt.Printf("    Firing: %d개\n", len(data.FiringAlerts))
	fmt.Printf("    Resolved: %d개\n", len(data.ResolvedAlerts))
	fmt.Printf("    공통 레이블: %v\n", data.CommonLabels)
}
