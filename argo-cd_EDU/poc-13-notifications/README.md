# PoC 13: 알림 시스템 (Notification System)

## 개요

Argo CD의 알림 시스템(`argocd-notifications`)을 Go 표준 라이브러리만으로 시뮬레이션합니다.

Application CR의 상태 변경을 감시하고, 구독 조건에 맞는 알림을 Slack/Email/Webhook으로 전송하는 전체 파이프라인을 구현합니다.

## 실행

```bash
go run main.go
```

## 핵심 개념

### 1. 어노테이션 기반 구독 (Subscription)

```
notifications.argoproj.io/subscribe.<trigger>.<service>=<recipient>
```

예시:
```yaml
annotations:
  notifications.argoproj.io/subscribe.on-sync-succeeded.slack: dev-alerts
  notifications.argoproj.io/subscribe.on-health-degraded.email: ops@example.com,sre@example.com
  notifications.argoproj.io/subscribe.on-deployed.webhook: https://hooks.example.com/deploy
```

- `<trigger>`: 트리거 이름 (on-sync-succeeded, on-health-degraded 등)
- `<service>`: 알림 서비스 (slack, email, webhook)
- `<recipient>`: 수신자 (채널명, 이메일, URL), 쉼표로 다중 지정 가능

### 2. 트리거 카탈로그 (Trigger Catalog)

사전 정의된 트리거:

| 트리거 | 조건 | oncePer |
|--------|------|---------|
| `on-sync-succeeded` | `operationState.phase == 'Succeeded'` | `operationState.finishedAt` |
| `on-sync-failed` | `operationState.phase == 'Failed' or 'Error'` | `operationState.finishedAt` |
| `on-health-degraded` | `health.status == 'Degraded'` | `health.status` |
| `on-deployed` | `phase == 'Succeeded' && health == 'Healthy'` | `operationState.finishedAt` |

`oncePer` 필드: 특정 값이 바뀌어야만 다시 알림을 보내는 dedup 기준

### 3. 템플릿 시스템 (Template System)

Go `text/template` 기반으로 알림 메시지를 렌더링합니다.

```
제목: Application {{.App.Name}} has been successfully synced
본문:
  Application *{{.App.Name}}* sync succeeded.
  • Project: {{.App.Spec.Project}}
  • Repo: {{.App.Spec.RepoURL}}
```

컨텍스트(`TemplateContext`)에 `App` 객체가 주입되어 모든 애플리케이션 필드에 접근 가능합니다.

### 4. 서비스 인터페이스 (Service Interface)

```go
type NotificationService interface {
    Name() string
    Send(notification Notification, recipient string) error
}
```

구현체:
- `SlackService`: Slack 채널로 전송
- `EmailService`: SMTP 이메일 전송
- `WebhookService`: HTTP Webhook 전송

### 5. 중복 방지 (Deduplication)

알림 전송 후 키를 캐시에 저장하여 동일한 이벤트에 대한 중복 전송을 방지합니다.

```
dedup key = sha256(appName + triggerName + oncePer + recipient)
```

실제 Argo CD에서는 이 키를 ConfigMap에 저장하여 컨트롤러 재시작 후에도 dedup이 유지됩니다.

## 알림 처리 흐름

```
Application 상태 변경
        │
        ▼
ParseSubscriptions(annotations)
        │
        ▼
    구독 목록 순회
        │
        ├── trigger.Condition.Evaluate(app) → false → 건너뜀
        │
        ├── oncePer 값 계산
        │
        ├── cache.IsSent(dedupKey) → true → 건너뜀 (dedup)
        │
        ├── template.Render(app) → title, body
        │
        └── service.Send(notification, recipient)
                │
                ├── SlackService
                ├── EmailService
                └── WebhookService
```

## 시뮬레이션 시나리오

| 시나리오 | 내용 |
|----------|------|
| 1 | 배포 성공 (Synced + Healthy) — Slack/Email 알림 |
| 2 | 헬스 Degraded — Slack + Webhook 알림 |
| 3 | 동기화 실패 — 다중 수신자 (slack 2개, email 1개) |
| 4 | 동일 상태 재처리 — Deduplication으로 건너뜀 |
| 5 | 새 동기화 완료 — oncePer 변경으로 새 알림 전송 |

## 실제 Argo CD 코드 참조

| 구성요소 | 소스 위치 |
|----------|-----------|
| NotificationController | `notification/controller.go` |
| 트리거 평가 | `notification/trigger.go` |
| 템플릿 렌더링 | `notification/template.go` |
| 서비스 인터페이스 | `notification/service/` |
| 구독 파싱 | `notification/subscriptions.go` |
| Dedup 캐시 | `notification/notificationsapi/cache.go` |
