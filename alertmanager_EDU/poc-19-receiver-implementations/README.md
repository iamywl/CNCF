# PoC: Alertmanager Receiver 구현체 시뮬레이션

## 개요

Alertmanager의 알림 전송을 담당하는 Receiver 구현체(Slack, Webhook, Email, PagerDuty)를
시뮬레이션한다.

## 대응하는 Alertmanager 소스코드

| 이 PoC | Alertmanager 소스 | 설명 |
|--------|------------------|------|
| `Notifier` 인터페이스 | `notify/notify.go` | 공통 알림 인터페이스 |
| `Retrier` | `notify/notify.go` | HTTP 상태코드별 재시도 판단 |
| `SlackNotifier` | `notify/slack/slack.go` | Slack Block Kit 알림 |
| `WebhookNotifier` | `notify/webhook/webhook.go` | 일반 HTTP 웹훅 |
| `EmailNotifier` | `notify/email/email.go` | SMTP 이메일 알림 |
| `PagerDutyNotifier` | `notify/pagerduty/pagerduty.go` | PagerDuty Events API |

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- 모든 Receiver는 동일한 Notifier 인터페이스를 구현한다
- Retrier로 일시적 오류(429, 5xx)만 재시도한다
- 템플릿으로 알림 메시지를 동적 생성한다
