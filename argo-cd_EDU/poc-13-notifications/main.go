// poc-13-notifications/main.go
//
// Argo CD 알림 시스템 시뮬레이션
//
// 핵심 개념:
//   - NotificationController: Application CR 변경 감시
//   - Trigger: 애플리케이션 상태 조건 평가
//   - Template: Go 템플릿 기반 메시지 렌더링
//   - Service: 알림 전송 (Slack, Email, Webhook)
//   - Subscription: 어노테이션 기반 구독
//   - Deduplication: 중복 알림 방지
//
// 실행: go run main.go

package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"text/template"
	"time"
)

// ============================================================
// 도메인 모델
// ============================================================

// HealthStatus 애플리케이션 헬스 상태
type HealthStatus string

const (
	HealthHealthy     HealthStatus = "Healthy"
	HealthDegraded    HealthStatus = "Degraded"
	HealthProgressing HealthStatus = "Progressing"
	HealthMissing     HealthStatus = "Missing"
	HealthUnknown     HealthStatus = "Unknown"
)

// SyncPhase 동기화 단계
type SyncPhase string

const (
	SyncPhaseSucceeded SyncPhase = "Succeeded"
	SyncPhaseFailed    SyncPhase = "Failed"
	SyncPhaseRunning   SyncPhase = "Running"
	SyncPhaseError     SyncPhase = "Error"
)

// SyncStatus 동기화 상태
type SyncStatus string

const (
	SyncStatusSynced    SyncStatus = "Synced"
	SyncStatusOutOfSync SyncStatus = "OutOfSync"
)

// OperationState 마지막 동기화 작업 결과
type OperationState struct {
	Phase      SyncPhase
	StartedAt  time.Time
	FinishedAt time.Time
	Message    string
}

// ApplicationHealth 애플리케이션 헬스 정보
type ApplicationHealth struct {
	Status  HealthStatus
	Message string
}

// ApplicationStatus 애플리케이션 상태 전체
type ApplicationStatus struct {
	Health         ApplicationHealth
	Sync           SyncStatus
	OperationState *OperationState
	ReconciledAt   time.Time
}

// ApplicationSpec 애플리케이션 스펙
type ApplicationSpec struct {
	Project         string
	RepoURL         string
	TargetRevision  string
	Path            string
	DestinationName string
	Namespace       string
}

// Application Argo CD Application CR 시뮬레이션
type Application struct {
	Name        string
	Namespace   string
	Annotations map[string]string
	Labels      map[string]string
	Spec        ApplicationSpec
	Status      ApplicationStatus
}

// GetAnnotation 어노테이션 값 조회
func (a *Application) GetAnnotation(key string) string {
	if v, ok := a.Annotations[key]; ok {
		return v
	}
	return ""
}

// ============================================================
// 구독 (Subscription) — 어노테이션 기반
// ============================================================
//
// 어노테이션 형식:
//   notifications.argoproj.io/subscribe.<trigger>.<service>=<recipient>
//
// 예시:
//   notifications.argoproj.io/subscribe.on-sync-succeeded.slack=dev-alerts
//   notifications.argoproj.io/subscribe.on-health-degraded.email=ops@example.com

const subscriptionAnnotationPrefix = "notifications.argoproj.io/subscribe."

// Subscription 단일 구독 정보
type Subscription struct {
	Trigger   string
	Service   string
	Recipient string
}

// ParseSubscriptions 어노테이션에서 구독 목록 파싱
func ParseSubscriptions(annotations map[string]string) []Subscription {
	var subs []Subscription
	for key, recipient := range annotations {
		if !strings.HasPrefix(key, subscriptionAnnotationPrefix) {
			continue
		}
		// "notifications.argoproj.io/subscribe.<trigger>.<service>" 파싱
		rest := key[len(subscriptionAnnotationPrefix):]
		// 마지막 '.' 기준으로 trigger와 service 분리
		lastDot := strings.LastIndex(rest, ".")
		if lastDot < 0 {
			continue
		}
		trigger := rest[:lastDot]
		service := rest[lastDot+1:]
		// 쉼표로 구분된 수신자 처리
		for _, r := range strings.Split(recipient, ",") {
			r = strings.TrimSpace(r)
			if r != "" {
				subs = append(subs, Subscription{
					Trigger:   trigger,
					Service:   service,
					Recipient: r,
				})
			}
		}
	}
	return subs
}

// ============================================================
// Trigger (트리거) — 조건 평가
// ============================================================

// TriggerCondition 트리거 조건
type TriggerCondition struct {
	// 조건 평가 함수 (실제 Argo CD에서는 expr 라이브러리 사용)
	Evaluate func(app *Application) bool
	// oncePer: 특정 조건 기반 dedup 키 (예: operationState.finishedAt)
	OncePer func(app *Application) string
}

// Trigger 트리거 정의
type Trigger struct {
	Name        string
	Description string
	Condition   TriggerCondition
	Template    string // 사용할 템플릿 이름
}

// buildTriggerCatalog 사전 정의된 트리거 카탈로그 구성
func buildTriggerCatalog() map[string]*Trigger {
	return map[string]*Trigger{
		"on-sync-succeeded": {
			Name:        "on-sync-succeeded",
			Description: "동기화 성공 시 알림",
			Condition: TriggerCondition{
				Evaluate: func(app *Application) bool {
					if app.Status.OperationState == nil {
						return false
					}
					return app.Status.OperationState.Phase == SyncPhaseSucceeded
				},
				OncePer: func(app *Application) string {
					if app.Status.OperationState == nil {
						return ""
					}
					return app.Status.OperationState.FinishedAt.Format(time.RFC3339)
				},
			},
			Template: "app-sync-succeeded",
		},
		"on-sync-failed": {
			Name:        "on-sync-failed",
			Description: "동기화 실패 시 알림",
			Condition: TriggerCondition{
				Evaluate: func(app *Application) bool {
					if app.Status.OperationState == nil {
						return false
					}
					return app.Status.OperationState.Phase == SyncPhaseFailed ||
						app.Status.OperationState.Phase == SyncPhaseError
				},
				OncePer: func(app *Application) string {
					if app.Status.OperationState == nil {
						return ""
					}
					return app.Status.OperationState.FinishedAt.Format(time.RFC3339)
				},
			},
			Template: "app-sync-failed",
		},
		"on-health-degraded": {
			Name:        "on-health-degraded",
			Description: "헬스 상태가 Degraded로 변경 시 알림",
			Condition: TriggerCondition{
				Evaluate: func(app *Application) bool {
					return app.Status.Health.Status == HealthDegraded
				},
				OncePer: func(app *Application) string {
					return string(app.Status.Health.Status)
				},
			},
			Template: "app-health-degraded",
		},
		"on-deployed": {
			Name:        "on-deployed",
			Description: "배포 완료(Synced + Healthy) 시 알림",
			Condition: TriggerCondition{
				Evaluate: func(app *Application) bool {
					if app.Status.OperationState == nil {
						return false
					}
					return app.Status.OperationState.Phase == SyncPhaseSucceeded &&
						app.Status.Health.Status == HealthHealthy
				},
				OncePer: func(app *Application) string {
					if app.Status.OperationState == nil {
						return ""
					}
					return app.Status.OperationState.FinishedAt.Format(time.RFC3339)
				},
			},
			Template: "app-deployed",
		},
	}
}

// ============================================================
// Template (템플릿) — Go 템플릿 기반 메시지 렌더링
// ============================================================

// NotificationTemplate 알림 템플릿
type NotificationTemplate struct {
	Name    string
	Title   string // 슬랙 제목 등
	Body    string // 본문 Go 템플릿
	Subject string // 이메일 제목 Go 템플릿
}

// TemplateContext 템플릿 렌더링 컨텍스트
type TemplateContext struct {
	App *Application
}

// Render 템플릿을 애플리케이션 컨텍스트로 렌더링
func (t *NotificationTemplate) Render(app *Application) (title, body string, err error) {
	ctx := TemplateContext{App: app}

	// 제목 렌더링
	titleTmpl, err := template.New("title").Parse(t.Title)
	if err != nil {
		return "", "", fmt.Errorf("제목 템플릿 파싱 오류: %w", err)
	}
	var titleBuf bytes.Buffer
	if err := titleTmpl.Execute(&titleBuf, ctx); err != nil {
		return "", "", fmt.Errorf("제목 템플릿 렌더링 오류: %w", err)
	}

	// 본문 렌더링
	bodyTmpl, err := template.New("body").Parse(t.Body)
	if err != nil {
		return "", "", fmt.Errorf("본문 템플릿 파싱 오류: %w", err)
	}
	var bodyBuf bytes.Buffer
	if err := bodyTmpl.Execute(&bodyBuf, ctx); err != nil {
		return "", "", fmt.Errorf("본문 템플릿 렌더링 오류: %w", err)
	}

	return titleBuf.String(), bodyBuf.String(), nil
}

// buildTemplateCatalog 사전 정의된 템플릿 카탈로그 구성
func buildTemplateCatalog() map[string]*NotificationTemplate {
	return map[string]*NotificationTemplate{
		"app-sync-succeeded": {
			Name:  "app-sync-succeeded",
			Title: `Application {{.App.Name}} has been successfully synced`,
			Body: `Application *{{.App.Name}}* sync succeeded.
• Project: {{.App.Spec.Project}}
• Repo: {{.App.Spec.RepoURL}}
• Revision: {{.App.Spec.TargetRevision}}
• Destination: {{.App.Spec.DestinationName}}/{{.App.Spec.Namespace}}
• Sync Phase: {{.App.Status.OperationState.Phase}}`,
		},
		"app-sync-failed": {
			Name:  "app-sync-failed",
			Title: `Application {{.App.Name}} sync failed`,
			Body: `Application *{{.App.Name}}* sync FAILED.
• Project: {{.App.Spec.Project}}
• Phase: {{.App.Status.OperationState.Phase}}
• Message: {{.App.Status.OperationState.Message}}`,
		},
		"app-health-degraded": {
			Name:  "app-health-degraded",
			Title: `Application {{.App.Name}} health is Degraded`,
			Body: `Application *{{.App.Name}}* health degraded.
• Health: {{.App.Status.Health.Status}}
• Message: {{.App.Status.Health.Message}}
• Sync: {{.App.Status.Sync}}`,
		},
		"app-deployed": {
			Name:  "app-deployed",
			Title: `Application {{.App.Name}} successfully deployed`,
			Body: `Application *{{.App.Name}}* deployed and healthy!
• Project: {{.App.Spec.Project}}
• Health: {{.App.Status.Health.Status}}
• Sync: {{.App.Status.Sync}}`,
		},
	}
}

// ============================================================
// Service (서비스) — 알림 전송
// ============================================================

// Notification 알림 데이터
type Notification struct {
	Title string
	Body  string
}

// NotificationService 알림 서비스 인터페이스
type NotificationService interface {
	Name() string
	Send(notification Notification, recipient string) error
}

// SlackService Slack 알림 서비스
type SlackService struct {
	// 실제에서는 WebhookURL, Token 등 설정
	WebhookURL string
}

func (s *SlackService) Name() string { return "slack" }

func (s *SlackService) Send(n Notification, recipient string) error {
	fmt.Printf("  [Slack] 채널: #%s\n", recipient)
	fmt.Printf("         제목: %s\n", n.Title)
	fmt.Printf("         본문: %s\n", strings.ReplaceAll(n.Body, "\n", "\n                "))
	return nil
}

// EmailService 이메일 알림 서비스
type EmailService struct {
	SMTPHost string
	From     string
}

func (s *EmailService) Name() string { return "email" }

func (s *EmailService) Send(n Notification, recipient string) error {
	fmt.Printf("  [Email] 수신자: %s\n", recipient)
	fmt.Printf("          제목: %s\n", n.Title)
	fmt.Printf("          본문: %s\n", strings.ReplaceAll(n.Body, "\n", "\n                 "))
	return nil
}

// WebhookService Webhook 알림 서비스
type WebhookService struct {
	URL    string
	Method string
}

func (s *WebhookService) Name() string { return "webhook" }

func (s *WebhookService) Send(n Notification, recipient string) error {
	fmt.Printf("  [Webhook] URL: %s\n", recipient)
	fmt.Printf("            제목: %s\n", n.Title)
	fmt.Printf("            본문: %s\n", strings.ReplaceAll(n.Body, "\n", "\n                  "))
	return nil
}

// ============================================================
// Deduplication (중복 방지)
// ============================================================
//
// 실제 Argo CD: ConfigMap에 알림 전송 기록을 저장하여 재시작 후에도 유지.
// 키: sha256(appName + triggerName + oncePer)
// 값: 마지막 전송 시각

// SentNotification 전송된 알림 기록
type SentNotification struct {
	Key       string
	SentAt    time.Time
	AppName   string
	Trigger   string
	Recipient string
}

// NotificationCache 알림 전송 이력 캐시 (실제: ConfigMap)
type NotificationCache struct {
	sent map[string]*SentNotification
}

// NewNotificationCache 알림 캐시 생성
func NewNotificationCache() *NotificationCache {
	return &NotificationCache{sent: make(map[string]*SentNotification)}
}

// buildKey 중복 방지 키 생성
func buildKey(appName, triggerName, oncePer, recipient string) string {
	data := fmt.Sprintf("%s|%s|%s|%s", appName, triggerName, oncePer, recipient)
	h := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", h[:8]) // 앞 8바이트만 사용
}

// IsSent 이미 전송되었는지 확인
func (c *NotificationCache) IsSent(key string) bool {
	_, ok := c.sent[key]
	return ok
}

// MarkSent 전송 완료 기록
func (c *NotificationCache) MarkSent(key string, record *SentNotification) {
	c.sent[key] = record
}

// GetHistory 전송 이력 반환
func (c *NotificationCache) GetHistory() []*SentNotification {
	var history []*SentNotification
	for _, v := range c.sent {
		history = append(history, v)
	}
	return history
}

// ============================================================
// NotificationController
// ============================================================

// NotificationController 알림 컨트롤러
type NotificationController struct {
	triggers  map[string]*Trigger
	templates map[string]*NotificationTemplate
	services  map[string]NotificationService
	cache     *NotificationCache
}

// NewNotificationController 알림 컨트롤러 생성
func NewNotificationController() *NotificationController {
	nc := &NotificationController{
		triggers:  buildTriggerCatalog(),
		templates: buildTemplateCatalog(),
		services:  make(map[string]NotificationService),
		cache:     NewNotificationCache(),
	}
	// 서비스 등록
	nc.services["slack"] = &SlackService{WebhookURL: "https://hooks.slack.com/..."}
	nc.services["email"] = &EmailService{SMTPHost: "smtp.example.com", From: "argocd@example.com"}
	nc.services["webhook"] = &WebhookService{URL: "https://webhook.site/...", Method: "POST"}
	return nc
}

// ProcessApplication 애플리케이션 상태 변경 처리
func (nc *NotificationController) ProcessApplication(app *Application) {
	fmt.Printf("\n[NotificationController] 앱 처리: %s\n", app.Name)

	// 어노테이션에서 구독 파싱
	subscriptions := ParseSubscriptions(app.Annotations)
	if len(subscriptions) == 0 {
		fmt.Println("  구독 없음 — 알림 전송 건너뜀")
		return
	}
	fmt.Printf("  구독 수: %d\n", len(subscriptions))

	for _, sub := range subscriptions {
		trigger, ok := nc.triggers[sub.Trigger]
		if !ok {
			fmt.Printf("  [WARN] 알 수 없는 트리거: %s\n", sub.Trigger)
			continue
		}

		// 1. 조건 평가
		if !trigger.Condition.Evaluate(app) {
			fmt.Printf("  트리거 [%s]: 조건 불일치 — 건너뜀\n", sub.Trigger)
			continue
		}

		// 2. oncePer 키 계산
		oncePer := ""
		if trigger.Condition.OncePer != nil {
			oncePer = trigger.Condition.OncePer(app)
		}

		// 3. 중복 확인
		dedupKey := buildKey(app.Name, sub.Trigger, oncePer, sub.Recipient)
		if nc.cache.IsSent(dedupKey) {
			fmt.Printf("  트리거 [%s] → 서비스 [%s] → 수신자 [%s]: 이미 전송됨 (dedup)\n",
				sub.Trigger, sub.Service, sub.Recipient)
			continue
		}

		// 4. 서비스 조회
		svc, ok := nc.services[sub.Service]
		if !ok {
			fmt.Printf("  [WARN] 알 수 없는 서비스: %s\n", sub.Service)
			continue
		}

		// 5. 템플릿 렌더링
		tmpl, ok := nc.templates[trigger.Template]
		if !ok {
			fmt.Printf("  [WARN] 템플릿 없음: %s\n", trigger.Template)
			continue
		}
		title, body, err := tmpl.Render(app)
		if err != nil {
			fmt.Printf("  [ERROR] 템플릿 렌더링 실패: %v\n", err)
			continue
		}

		// 6. 알림 전송
		fmt.Printf("  트리거 [%s] → 서비스 [%s] → 수신자 [%s]:\n",
			sub.Trigger, sub.Service, sub.Recipient)
		notification := Notification{Title: title, Body: body}
		if err := svc.Send(notification, sub.Recipient); err != nil {
			fmt.Printf("  [ERROR] 전송 실패: %v\n", err)
			continue
		}

		// 7. 전송 기록 저장
		nc.cache.MarkSent(dedupKey, &SentNotification{
			Key:       dedupKey,
			SentAt:    time.Now(),
			AppName:   app.Name,
			Trigger:   sub.Trigger,
			Recipient: sub.Recipient,
		})
	}
}

// ============================================================
// 시뮬레이션 실행
// ============================================================

func main() {
	fmt.Println("============================================================")
	fmt.Println("  Argo CD 알림 시스템 시뮬레이션 (PoC-13)")
	fmt.Println("============================================================")

	// 알림 컨트롤러 초기화
	controller := NewNotificationController()

	// ----------------------------------------------------------------
	// 테스트 애플리케이션 정의
	// ----------------------------------------------------------------
	now := time.Now()

	// [앱 1] 동기화 성공 + 배포 완료 상태
	appDeployed := &Application{
		Name:      "guestbook",
		Namespace: "argocd",
		Annotations: map[string]string{
			"notifications.argoproj.io/subscribe.on-sync-succeeded.slack": "dev-alerts",
			"notifications.argoproj.io/subscribe.on-deployed.slack":       "release-channel",
			"notifications.argoproj.io/subscribe.on-deployed.email":       "ops@example.com",
		},
		Spec: ApplicationSpec{
			Project:         "default",
			RepoURL:         "https://github.com/argoproj/argocd-example-apps.git",
			TargetRevision:  "HEAD",
			Path:            "guestbook",
			DestinationName: "in-cluster",
			Namespace:       "default",
		},
		Status: ApplicationStatus{
			Health: ApplicationHealth{Status: HealthHealthy, Message: ""},
			Sync:   SyncStatusSynced,
			OperationState: &OperationState{
				Phase:      SyncPhaseSucceeded,
				StartedAt:  now.Add(-2 * time.Minute),
				FinishedAt: now.Add(-1 * time.Minute),
				Message:    "successfully synced (all tasks run)",
			},
		},
	}

	// [앱 2] 헬스 Degraded 상태
	appDegraded := &Application{
		Name:      "backend-api",
		Namespace: "argocd",
		Annotations: map[string]string{
			"notifications.argoproj.io/subscribe.on-health-degraded.slack":   "alerts",
			"notifications.argoproj.io/subscribe.on-health-degraded.webhook": "https://pagerduty.example.com/hook",
		},
		Spec: ApplicationSpec{
			Project:         "production",
			RepoURL:         "https://github.com/myorg/backend-api.git",
			TargetRevision:  "v2.3.1",
			Path:            "helm",
			DestinationName: "prod-cluster",
			Namespace:       "backend",
		},
		Status: ApplicationStatus{
			Health: ApplicationHealth{
				Status:  HealthDegraded,
				Message: "Deployment backend-api: 0/3 pods are ready",
			},
			Sync: SyncStatusSynced,
		},
	}

	// [앱 3] 동기화 실패
	appFailed := &Application{
		Name:      "frontend",
		Namespace: "argocd",
		Annotations: map[string]string{
			"notifications.argoproj.io/subscribe.on-sync-failed.slack": "dev-alerts,frontend-team",
			"notifications.argoproj.io/subscribe.on-sync-failed.email": "frontend@example.com",
		},
		Spec: ApplicationSpec{
			Project:         "default",
			RepoURL:         "https://github.com/myorg/frontend.git",
			TargetRevision:  "main",
			Path:            "k8s",
			DestinationName: "in-cluster",
			Namespace:       "frontend",
		},
		Status: ApplicationStatus{
			Health: ApplicationHealth{Status: HealthUnknown, Message: ""},
			Sync:   SyncStatusOutOfSync,
			OperationState: &OperationState{
				Phase:      SyncPhaseFailed,
				StartedAt:  now.Add(-5 * time.Minute),
				FinishedAt: now.Add(-4 * time.Minute),
				Message:    "failed to create resource: ConfigMap \"app-config\" is invalid",
			},
		},
	}

	// ----------------------------------------------------------------
	// 시나리오 1: 정상 배포
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 1: 배포 성공 (Synced + Healthy)")
	fmt.Println("============================")
	controller.ProcessApplication(appDeployed)

	// ----------------------------------------------------------------
	// 시나리오 2: 헬스 Degraded
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 2: 헬스 Degraded")
	fmt.Println("============================")
	controller.ProcessApplication(appDegraded)

	// ----------------------------------------------------------------
	// 시나리오 3: 동기화 실패 (다중 수신자)
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 3: 동기화 실패 (다중 수신자)")
	fmt.Println("============================")
	controller.ProcessApplication(appFailed)

	// ----------------------------------------------------------------
	// 시나리오 4: 중복 알림 방지 (Deduplication)
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 4: 동일 앱 재처리 — Deduplication 동작")
	fmt.Println("============================")
	fmt.Println("(같은 상태로 앱을 다시 처리 → 이미 전송된 알림은 건너뜀)")
	controller.ProcessApplication(appDeployed)

	// ----------------------------------------------------------------
	// 시나리오 5: 앱 상태 변경 후 재전송
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 5: 새 동기화 작업 완료 — oncePer 변경으로 재전송")
	fmt.Println("============================")
	// operationState.FinishedAt이 달라지면 oncePer가 달라져 새 알림 전송
	appDeployed2 := *appDeployed
	appDeployed2.Status.OperationState = &OperationState{
		Phase:      SyncPhaseSucceeded,
		StartedAt:  now.Add(5 * time.Minute),
		FinishedAt: now.Add(6 * time.Minute), // 다른 시각
		Message:    "successfully synced",
	}
	controller.ProcessApplication(&appDeployed2)

	// ----------------------------------------------------------------
	// 전송 이력 출력
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("전송 이력 (Notification Cache)")
	fmt.Println("============================")
	history := controller.cache.GetHistory()
	fmt.Printf("총 전송 알림 수: %d\n", len(history))
	for i, h := range history {
		fmt.Printf("  [%d] App=%-15s Trigger=%-22s Recipient=%s\n",
			i+1, h.AppName, h.Trigger, h.Recipient)
	}

	// ----------------------------------------------------------------
	// 구독 파싱 상세 출력
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("어노테이션 기반 구독 파싱 예시")
	fmt.Println("============================")
	annotations := map[string]string{
		"notifications.argoproj.io/subscribe.on-sync-succeeded.slack":   "dev-alerts",
		"notifications.argoproj.io/subscribe.on-health-degraded.email":  "ops@example.com,sre@example.com",
		"notifications.argoproj.io/subscribe.on-deployed.webhook":       "https://hooks.example.com/deploy",
		"app.kubernetes.io/name":                                         "ignored-label",
	}
	subs := ParseSubscriptions(annotations)
	fmt.Printf("어노테이션 수: %d, 파싱된 구독 수: %d\n", len(annotations), len(subs))
	for _, s := range subs {
		fmt.Printf("  트리거=%-22s 서비스=%-8s 수신자=%s\n", s.Trigger, s.Service, s.Recipient)
	}

	fmt.Println("\n[완료] 알림 시스템 시뮬레이션 종료")
}
