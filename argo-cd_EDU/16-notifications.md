# 16. Argo CD Notifications 시스템

## 개요

Argo CD Notifications는 Application 상태 변화를 감지하고 다양한 외부 채널(Slack, Email, Webhook 등)에 알림을 전송하는 서브시스템이다. `argoproj/notifications-engine` 라이브러리를 기반으로 구현되어 있으며, Argo CD와 독립적으로 배포 가능한 별도 컨트롤러(`argocd-notifications-controller`)로 실행된다.

```
애플리케이션 상태 변화
        │
        ▼
┌──────────────────────────────────────────┐
│   argocd-notifications-controller        │
│                                          │
│  ┌─────────────┐   ┌──────────────────┐  │
│  │ App Informer│   │ ConfigMap/Secret  │  │
│  │ (K8s Watch) │   │    Informer       │  │
│  └──────┬──────┘   └────────┬─────────┘  │
│         │                  │            │
│         ▼                  ▼            │
│  ┌─────────────────────────────────┐    │
│  │  notifications-engine/controller│    │
│  │  ┌──────────┐  ┌────────────┐   │    │
│  │  │트리거 평가│  │구독자 조회 │   │    │
│  │  └──────────┘  └────────────┘   │    │
│  │  ┌──────────┐  ┌────────────┐   │    │
│  │  │템플릿 렌더│  │알림 전송   │   │    │
│  │  └──────────┘  └────────────┘   │    │
│  └─────────────────────────────────┘    │
└──────────────────────────────────────────┘
        │
        ▼
  Slack / Email / Webhook / PagerDuty / ...
```

---

## 1. Notification Controller 개요

### 1.1 진입점: argocd-notification 커맨드

`cmd/argocd-notification/commands/argocd_notification.go`에 컨트롤러 시작 로직이 있다.

```go
// cmd/argocd-notification/commands/argocd_notification.go

func NewCommand() *cobra.Command {
    var (
        processorsCount                int
        configMapName                  string   // 기본값: "argocd-notifications-cm"
        secretName                     string   // 기본값: "argocd-notifications-secret"
        applicationNamespaces          []string
        selfServiceNotificationEnabled bool
    )

    command := cobra.Command{
        Use:   common.CommandNotifications,
        Short: "Starts Argo CD Notifications controller",
        RunE: func(_ *cobra.Command, _ []string) error {
            // 1. Kubernetes 클라이언트 생성
            dynamicClient, _ := dynamic.NewForConfig(restConfig)
            k8sClient, _ := kubernetes.NewForConfig(restConfig)

            // 2. ArgoCD 서비스 초기화 (repo 메타데이터 접근용)
            argocdService, _ := service.NewArgoCDService(k8sClient, namespace, repoClientset)

            // 3. 컨트롤러 생성 및 초기화
            ctrl := notificationscontroller.NewController(...)
            ctrl.Init(ctx)

            // 4. 컨트롤러 실행
            go ctrl.Run(ctx, processorsCount)
            <-ctx.Done()
        },
    }
    command.Flags().StringVar(&configMapName, "config-map-name", "argocd-notifications-cm", ...)
    command.Flags().StringVar(&secretName, "secret-name", "argocd-notifications-secret", ...)
    command.Flags().BoolVar(&selfServiceNotificationEnabled, "self-service-notification-enabled", ...)
}
```

기본 메트릭 포트는 `9001`번이며, Prometheus 형식의 메트릭을 `/metrics` 엔드포인트로 노출한다.

### 1.2 설계 원칙

Notification Controller는 다음 세 가지 원칙으로 설계되어 있다.

| 원칙 | 설명 |
|------|------|
| **독립 배포** | Application Controller와 분리된 별도 Pod로 실행 |
| **엔진 재사용** | `argoproj/notifications-engine`을 라이브러리로 사용 |
| **선언적 설정** | ConfigMap과 Secret만으로 트리거/템플릿/서비스 설정 |

---

## 2. NotificationController 인터페이스와 구조체

### 2.1 인터페이스 정의

`notification_controller/controller/controller.go`에 인터페이스가 정의되어 있다.

```go
// notification_controller/controller/controller.go

type NotificationController interface {
    Run(ctx context.Context, processors int)
    Init(ctx context.Context) error
}
```

`Init`은 Informer 동기화를 담당하고, `Run`은 실제 처리 루프를 시작한다.

### 2.2 notificationController 구조체

```go
type notificationController struct {
    ctrl              controller.NotificationController  // notifications-engine 컨트롤러
    appInformer       cache.SharedIndexInformer          // Application 변경 감시
    appProjInformer   cache.SharedIndexInformer          // AppProject 조회 (구독 확장)
    secretInformer    cache.SharedIndexInformer          // argocd-notifications-secret 감시
    configMapInformer cache.SharedIndexInformer          // argocd-notifications-cm 감시
}
```

각 Informer의 역할:

| Informer | 감시 대상 | 목적 |
|----------|----------|------|
| `appInformer` | `Application` CR | 상태 변화 감지 → 트리거 평가 |
| `appProjInformer` | `AppProject` CR | 프로젝트 레벨 구독 조회 |
| `secretInformer` | `argocd-notifications-secret` | 서비스 자격증명 실시간 반영 |
| `configMapInformer` | `argocd-notifications-cm` | 트리거/템플릿 설정 실시간 반영 |

ConfigMap과 Secret Informer는 3분 주기(`settingsResyncDuration = 3 * time.Minute`)로 재동기화한다(`util/notification/k8s/informers.go`).

### 2.3 NewController 생성자

```go
func NewController(
    k8sClient kubernetes.Interface,
    client dynamic.Interface,
    argocdService service.Service,
    namespace string,
    applicationNamespaces []string,
    appLabelSelector string,
    registry *controller.MetricsRegistry,
    secretName string,
    configMapName string,
    selfServiceNotificationEnabled bool,
) *notificationController {

    // Application Informer: 멀티네임스페이스 지원
    namespaceableAppClient := client.Resource(applications)
    appClient = namespaceableAppClient.Namespace(namespace) // 단일 네임스페이스 모드

    appInformer := newInformer(appClient, namespace, applicationNamespaces, appLabelSelector)
    appProjInformer := newInformer(newAppProjClient(client, namespace), ...)

    // 설정 Informer: selfServiceNotification 활성화 시 전체 네임스페이스 감시
    notificationConfigNamespace := namespace
    if selfServiceNotificationEnabled {
        notificationConfigNamespace = metav1.NamespaceAll
    }
    secretInformer := k8s.NewSecretInformer(k8sClient, notificationConfigNamespace, secretName)
    configMapInformer := k8s.NewConfigMapInformer(k8sClient, notificationConfigNamespace, configMapName)

    // notifications-engine Factory: ConfigMap/Secret 변경 시 설정 자동 재로드
    apiFactory := api.NewFactory(
        settings.GetFactorySettings(argocdService, secretName, configMapName, ...),
        namespace,
        secretInformer,
        configMapInformer,
    )

    // Skip 조건: SyncStatus 갱신 전 처리 지연
    skipProcessingOpt := controller.WithSkipProcessing(func(obj metav1.Object) (bool, string) {
        return !isAppSyncStatusRefreshed(app, log.WithField("app", obj.GetName())),
               "sync status out of date"
    })

    // AlterDestinations: AppProject 기반 구독 병합
    alterDestinationsOpt := controller.WithAlterDestinations(res.alterDestinations)
}
```

### 2.4 Init과 Run

```go
func (c *notificationController) Init(ctx context.Context) error {
    // TLS 인증서 리졸버 설정
    httputil.SetCertResolver(argocert.GetCertificateForConnect)

    // 모든 Informer 병렬 시작
    go c.appInformer.Run(ctx.Done())
    go c.appProjInformer.Run(ctx.Done())
    go c.secretInformer.Run(ctx.Done())
    go c.configMapInformer.Run(ctx.Done())

    // 캐시 동기화 완료 대기
    if !cache.WaitForCacheSync(ctx.Done(),
        c.appInformer.HasSynced,
        c.appProjInformer.HasSynced,
        c.secretInformer.HasSynced,
        c.configMapInformer.HasSynced,
    ) {
        return errors.New("timed out waiting for caches to sync")
    }
    return nil
}

func (c *notificationController) Run(ctx context.Context, processors int) {
    // notifications-engine 컨트롤러에 위임
    c.ctrl.Run(processors, ctx.Done())
}
```

---

## 3. 설정: argocd-notifications-cm ConfigMap

Notifications의 모든 설정은 `argocd-notifications-cm` ConfigMap 하나에 집중된다.

### 3.1 ConfigMap 전체 구조

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-notifications-cm
  namespace: argocd
data:
  # 1. 서비스 설정
  service.slack: |
    token: $slack-token
    username: ArgoCD
    icon: ":rocket:"

  service.email: |
    host: smtp.gmail.com
    port: 587
    from: noreply@company.com
    username: $email-username
    password: $email-password

  # 2. 트리거 정의
  trigger.on-sync-succeeded: |
    - when: app.status.operationState != nil and app.status.operationState.phase in ['Succeeded']
      send: [app-sync-succeeded]
      oncePer: app.status.operationState?.syncResult?.revision

  trigger.on-health-degraded: |
    - when: app.status.health.status == 'Degraded'
      send: [app-health-degraded]
      oncePer: app.status.operationState?.syncResult?.revision

  # 3. 템플릿 정의
  template.app-sync-succeeded: |
    message: Application {{.app.metadata.name}} has been successfully synced.
    slack:
      attachments: |
        [{"title": "{{.app.metadata.name}}", "color": "#18be52"}]

  # 4. 기본 구독 (전역 적용)
  subscriptions: |
    - recipients:
      - slack:general
      triggers:
      - on-health-degraded

  # 5. 글로벌 컨텍스트 변수
  context: |
    argocdUrl: https://argocd.company.com
```

### 3.2 Secret: argocd-notifications-secret

민감한 자격증명은 별도 Secret에 보관한다.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: argocd-notifications-secret
  namespace: argocd
type: Opaque
stringData:
  slack-token: xoxb-your-slack-token
  email-username: noreply@company.com
  email-password: yourpassword
  webhook-token: your-webhook-token
```

ConfigMap에서 `$변수명` 형태로 Secret 값을 참조한다.

### 3.3 설정 키 네이밍 규칙

| 키 패턴 | 예시 | 의미 |
|---------|------|------|
| `service.<이름>` | `service.slack` | 서비스 설정 |
| `trigger.<이름>` | `trigger.on-sync-succeeded` | 트리거 정의 |
| `template.<이름>` | `template.app-sync-succeeded` | 템플릿 정의 |
| `subscriptions` | - | 기본 구독 목록 |
| `context` | - | 글로벌 변수 |
| `defaultTriggers` | - | 기본 트리거 목록 |

---

## 4. 트리거 시스템

### 4.1 트리거 구조

트리거는 `when` 조건(CEL/expr 표현식), `send` 템플릿 목록, `oncePer` 중복 방지 키로 구성된다.

```yaml
trigger.on-deployed: |
  - when: >
      app.status.operationState != nil
      and app.status.operationState.phase in ['Succeeded']
      and app.status.health.status == 'Healthy'
      and (
        !time.Parse(app.status.health.lastTransitionTime).Add(1 * time.Minute).Before(
          time.Parse(app.status.operationState.finishedAt)
        )
        or time.Parse(app.status.health.lastTransitionTime).Before(
          time.Parse(app.status.operationState.startedAt)
        )
      )
    description: Application is synced and healthy. Triggered once per commit.
    send: [app-deployed]
    oncePer: app.status.operationState?.syncResult?.revision
```

### 4.2 트리거 조건 평가 변수

트리거 `when` 절에서 사용할 수 있는 변수와 함수:

| 변수/함수 | 타입 | 예시 |
|----------|------|------|
| `app` | `map[string]any` | Application CR 전체 객체 |
| `app.metadata.name` | `string` | Application 이름 |
| `app.spec.project` | `string` | 소속 프로젝트 |
| `app.status.sync.status` | `string` | `Synced`, `OutOfSync` |
| `app.status.health.status` | `string` | `Healthy`, `Degraded`, `Progressing` |
| `app.status.operationState.phase` | `string` | `Succeeded`, `Failed`, `Error`, `Running` |
| `app.status.operationState.finishedAt` | `string` | 작업 완료 시간 (RFC3339) |
| `time.Parse(ts)` | `time.Time` | 시간 파싱 함수 |
| `time.Now()` | `time.Time` | 현재 시간 |

### 4.3 카탈로그 트리거 정의

`notifications_catalog/triggers/` 디렉토리에 사전 정의된 트리거가 있다.

| 파일명 | 트리거명 | 조건 |
|--------|---------|------|
| `on-created.yaml` | on-created | `when: true` (항상) |
| `on-deleted.yaml` | on-deleted | `app.metadata.deletionTimestamp != nil` |
| `on-deployed.yaml` | on-deployed | Succeeded + Healthy + 신규 커밋 |
| `on-health-degraded.yaml` | on-health-degraded | `health.status == 'Degraded'` |
| `on-sync-failed.yaml` | on-sync-failed | `phase in ['Error', 'Failed']` |
| `on-sync-running.yaml` | on-sync-running | `phase in ['Running']` |
| `on-sync-status-unknown.yaml` | on-sync-status-unknown | `sync.status == 'Unknown'` |
| `on-sync-succeeded.yaml` | on-sync-succeeded | `phase in ['Succeeded']` |

실제 YAML 내용 (`notifications_catalog/triggers/on-sync-succeeded.yaml`):

```yaml
- when: app.status.operationState != nil and app.status.operationState.phase in ['Succeeded']
  description: Application syncing has succeeded
  send: [app-sync-succeeded]
  oncePer: app.status.operationState?.syncResult?.revision
```

`oncePer` 필드: 동일 revision에 대해 알림을 한 번만 전송한다. Application annotation에 마지막으로 알림을 전송한 revision을 기록하여 중복을 방지한다.

### 4.4 SyncStatus 갱신 대기 로직

Operation이 완료된 후 SyncStatus가 즉시 갱신되지 않는 경우를 처리하기 위해 `isAppSyncStatusRefreshed` 함수가 있다.

```go
// notification_controller/controller/controller.go

func isAppSyncStatusRefreshed(app *unstructured.Unstructured, logEntry *log.Entry) bool {
    phase, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "phase")

    switch phase {
    case "Failed", "Error", "Succeeded":
        finishedAt, _ := time.Parse(time.RFC3339, finishedAtRaw)
        reconciledAt, _ := time.Parse(time.RFC3339, reconciledAtRaw)
        observedAt, _ := time.Parse(time.RFC3339, observedAtRaw)

        // finishedAt이 reconciledAt/observedAt보다 이후이면 아직 SyncStatus 미갱신
        if finishedAt.After(reconciledAt) && finishedAt.After(observedAt) {
            return false  // 처리 스킵 → 다음 재처리 시도 때까지 대기
        }
    }
    return true
}
```

이 로직이 없으면, Operation 완료 직후 SyncStatus가 갱신되기 전에 트리거가 평가되어 잘못된 상태로 알림이 전송될 수 있다.

---

## 5. 템플릿 시스템

### 5.1 Go Template 기반 렌더링

템플릿은 Go의 `text/template` 패키지 문법을 따른다. 트리거 평가와 동일한 변수 세트를 사용한다.

```yaml
template.app-sync-succeeded: |
  message: |
    {{if eq .serviceType "slack"}}:white_check_mark:{{end}}
    Application {{.app.metadata.name}} has been successfully synced at
    {{.app.status.operationState.finishedAt}}.
    Sync operation details are available at:
    {{.context.argocdUrl}}/applications/{{.app.metadata.name}}?operation=true .
  email:
    subject: Application {{.app.metadata.name}} has been successfully synced.
  slack:
    attachments: |
      [{
        "title": "{{ .app.metadata.name}}",
        "title_link": "{{.context.argocdUrl}}/applications/{{.app.metadata.name}}",
        "color": "#18be52",
        "fields": [
          {"title": "Sync Status", "value": "{{.app.status.sync.status}}", "short": true},
          {"title": "Repository",
           "value": "{{ .app.spec.source.repoURL }}", "short": true}
          {{range $index, $c := .app.status.conditions}},
          {"title": "{{$c.type}}", "value": "{{$c.message}}", "short": true}
          {{end}}
        ]
      }]
```

### 5.2 템플릿 변수 주입: expression.Spawn

`util/notification/expression/expr.go`의 `Spawn` 함수가 템플릿/트리거 평가 시 사용할 변수 맵을 구성한다.

```go
// util/notification/expression/expr.go

var helpers = map[string]any{}

func init() {
    helpers = make(map[string]any)
    register("time", time.NewExprs())      // time.Parse, time.Now 등
    register("strings", strings.NewExprs()) // strings.ToUpper 등
}

func Spawn(
    app *unstructured.Unstructured,
    argocdService service.Service,
    vars map[string]any,
) map[string]any {
    clone := make(map[string]any)
    for k := range vars {
        clone[k] = vars[k]
    }
    maps.Copy(clone, helpers)       // time, strings 네임스페이스 추가
    clone["repo"] = repo.NewExprs(argocdService, app) // repo 함수 추가
    return clone
}
```

### 5.3 내장 함수 네임스페이스

**time 네임스페이스** (`util/notification/expression/time/time.go`):

```go
func NewExprs() map[string]any {
    return map[string]any{
        "Parse": parse,           // time.Parse(RFC3339) → time.Time
        "Now":   time.Now,        // 현재 시간 반환
        // 상수들
        "Second":  time.Second,
        "Minute":  time.Minute,
        "Hour":    time.Hour,
        "RFC3339": time.RFC3339,
        // ...
    }
}
```

트리거 표현식에서 사용 예:
```
time.Parse(app.status.health.lastTransitionTime).Add(1 * time.Minute).Before(time.Now())
```

**strings 네임스페이스** (`util/notification/expression/strings/strings.go`):

```go
func NewExprs() map[string]any {
    return map[string]any{
        "ReplaceAll": strings.ReplaceAll,
        "ToUpper":    strings.ToUpper,
        "ToLower":    strings.ToLower,
    }
}
```

**repo 네임스페이스** (`util/notification/expression/repo/repo.go`):

```go
func NewExprs(argocdService service.Service, app *unstructured.Unstructured) map[string]any {
    return map[string]any{
        "RepoURLToHTTPS":    repoURLToHTTPS,    // SSH URL → HTTPS 변환
        "FullNameByRepoURL": FullNameByRepoURL, // "org/repo" 추출
        "QueryEscape":       url.QueryEscape,   // URL 인코딩
        "GetCommitMetadata": func(commitSHA string) any {
            // repo-server를 통해 커밋 메시지, 작성자, 태그 조회
            meta, _ := getCommitMetadata(commitSHA, app, argocdService)
            return *meta
        },
        "GetAppDetails": func() any {
            // repo-server를 통해 앱 상세(Helm values 등) 조회
            appDetails, _ := getAppDetails(app, argocdService)
            return *appDetails
        },
    }
}
```

### 5.4 서비스별 템플릿 섹션

하나의 템플릿 YAML에 여러 서비스별 섹션을 정의할 수 있다.

| 섹션 | 설명 |
|------|------|
| `message` | 모든 서비스 공통 메시지 (fallback) |
| `email.subject` | 이메일 제목 |
| `slack.attachments` | Slack 첨부 JSON |
| `slack.blocks` | Slack Block Kit JSON |
| `teams.title` | Microsoft Teams 제목 |
| `teams.facts` | Teams fact 목록 JSON |
| `teams.themeColor` | Teams 카드 색상 |
| `webhook.method` | HTTP 메서드 |
| `webhook.body` | 요청 바디 |

`.serviceType` 변수를 통해 현재 전송 대상 서비스 타입을 알 수 있어 조건부 렌더링이 가능하다:

```
{{if eq .serviceType "slack"}}:white_check_mark:{{end}}
```

---

## 6. 지원 서비스 (알림 채널)

notifications-engine이 지원하는 서비스 목록:

| 서비스 | ConfigMap 키 | 설명 |
|--------|-------------|------|
| **Slack** | `service.slack` | 채널/DM 메시지, Attachment, Block Kit 지원 |
| **Email** | `service.email` | SMTP 이메일 전송 |
| **Webhook** | `service.webhook.<이름>` | 커스텀 HTTP POST/GET 호출 |
| **PagerDuty** | `service.pagerduty` | 인시던트 생성/해결 |
| **Microsoft Teams** | `service.teams` | Adaptive Card 메시지 |
| **Telegram** | `service.telegram` | 봇 메시지 |
| **GitHub** | `service.github` | 커밋 상태(Status Check) 업데이트 |
| **Grafana** | `service.grafana` | 대시보드 어노테이션 추가 |
| **Opsgenie** | `service.opsgenie` | 알림(Alert) 생성/종료 |
| **Mattermost** | `service.mattermost` | 채널 메시지 |
| **Rocketchat** | `service.rocketchat` | 채널 메시지 |
| **GoogleChat** | `service.googlechat` | Space 메시지 |
| **Alertmanager** | `service.alertmanager` | Alert 발송 |

### 6.1 Slack 서비스 설정 예시

```yaml
# argocd-notifications-cm ConfigMap
service.slack: |
  token: $slack-token
  username: ArgoCD Bot
  icon: ":rocket:"
  signingSecret: $slack-signing-secret  # 선택사항

# argocd-notifications-secret Secret
slack-token: xoxb-xxxxxxxxxxxx-xxxxxxxxxxxxxxxxxx
```

### 6.2 Email 서비스 설정 예시

```yaml
service.email: |
  host: smtp.gmail.com
  port: 587
  from: argocd@company.com
  username: $email-username
  password: $email-password
  # TLS 설정 (선택사항)
  tlsInsecureSkipVerify: false
```

### 6.3 Webhook 서비스 설정 예시

```yaml
service.webhook.teams-webhook: |
  url: https://outlook.office.com/webhook/xxx/IncomingWebhook/yyy/zzz
  headers:
  - name: Content-Type
    value: application/json
```

### 6.4 GitHub 커밋 상태 서비스 설정 예시

```yaml
service.github: |
  appID: 12345
  installationID: 67890
  privateKey: $github-privateKey
```

GitHub 서비스를 사용하면 Pull Request에 Argo CD 배포 상태가 체크 표시로 나타난다.

---

## 7. 구독 메커니즘

### 7.1 Application Annotation 기반 구독

가장 기본적인 구독 방법. Application CR의 annotation에 직접 지정한다.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  annotations:
    # 형식: notifications.argoproj.io/subscribe.<trigger>.<service>=<recipient>
    notifications.argoproj.io/subscribe.on-sync-succeeded.slack: my-channel
    notifications.argoproj.io/subscribe.on-health-degraded.slack: ops-alerts
    notifications.argoproj.io/subscribe.on-sync-failed.email: team@company.com
    notifications.argoproj.io/subscribe.on-deployed.slack: deployments|#releases
```

복수의 수신자는 `|`로 구분한다:
```
notifications.argoproj.io/subscribe.on-deployed.slack: channel1|channel2
```

### 7.2 AppProject 기반 구독

AppProject에 annotation을 추가하면 해당 프로젝트의 모든 Application에 구독이 적용된다.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: production
  annotations:
    notifications.argoproj.io/subscribe.on-health-degraded.slack: prod-alerts
    notifications.argoproj.io/subscribe.on-sync-failed.pagerduty: prod-oncall
```

`alterDestinations` 함수가 AppProject annotation을 읽어 구독 목록에 병합한다:

```go
// notification_controller/controller/controller.go

func (c *notificationController) alterDestinations(
    obj metav1.Object,
    destinations services.Destinations,
    cfg api.Config,
) services.Destinations {
    app, _ := (obj).(*unstructured.Unstructured)
    if proj := getAppProj(app, c.appProjInformer); proj != nil {
        // AppProject annotation에서 구독 읽기
        destinations.Merge(
            subscriptions.NewAnnotations(proj.GetAnnotations()).
                GetDestinations(cfg.DefaultTriggers, cfg.ServiceDefaultTriggers),
        )
        // 레거시 annotation 형식도 지원
        destinations.Merge(
            settings.GetLegacyDestinations(proj.GetAnnotations(), ...),
        )
    }
    return destinations
}
```

### 7.3 ConfigMap 글로벌 기본 구독

`argocd-notifications-cm`의 `subscriptions` 키로 전체 Application에 적용되는 기본 구독을 설정한다.

```yaml
subscriptions: |
  # 모든 Application에 적용
  - recipients:
    - slack:ops-general
    triggers:
    - on-health-degraded
    - on-sync-failed

  # 특정 레이블을 가진 Application에만 적용 (labels 필터)
  - recipients:
    - slack:prod-alerts
    triggers:
    - on-deployed
    selector: env=production
```

### 7.4 구독 우선순위 및 병합

구독은 여러 레벨에서 병합된다:

```
ConfigMap 기본 구독 (subscriptions)
        +
AppProject annotation 구독
        +
Application annotation 구독
        =
최종 알림 전송 대상 목록
```

동일 트리거+서비스+수신자 조합은 중복 제거된다.

---

## 8. notifications_catalog/

### 8.1 카탈로그 구조

```
notifications_catalog/
├── install.yaml              # 모든 기본 트리거 + 템플릿을 포함한 단일 ConfigMap
├── triggers/
│   ├── on-created.yaml
│   ├── on-deleted.yaml
│   ├── on-deployed.yaml
│   ├── on-health-degraded.yaml
│   ├── on-sync-failed.yaml
│   ├── on-sync-running.yaml
│   ├── on-sync-status-unknown.yaml
│   └── on-sync-succeeded.yaml
└── templates/
    ├── app-created.yaml
    ├── app-deleted.yaml
    ├── app-deployed.yaml
    ├── app-health-degraded.yaml
    ├── app-sync-failed.yaml
    ├── app-sync-running.yaml
    ├── app-sync-status-unknown.yaml
    └── app-sync-succeeded.yaml
```

### 8.2 카탈로그 설치

```bash
# notifications_catalog/install.yaml을 적용하면 기본 트리거와 템플릿이 설치된다
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/notifications_catalog/install.yaml
```

### 8.3 카탈로그 생성 스크립트

`hack/gen-catalog/main.go`가 개별 YAML 파일들을 읽어 `install.yaml`을 생성한다.

### 8.4 Slack 전용 app-deployed 템플릿 내용 분석

```yaml
# notifications_catalog/templates/app-deployed.yaml

message: |
  {{if eq .serviceType "slack"}}:white_check_mark:{{end}}
  Application {{.app.metadata.name}} is now running new version of deployments manifests.

slack:
  attachments: |
    [{
      "title": "{{ .app.metadata.name}}",
      "title_link": "{{.context.argocdUrl}}/applications/{{.app.metadata.name}}",
      "color": "#18be52",
      "fields": [
        {"title": "Sync Status", "value": "{{.app.status.sync.status}}", "short": true},
        {
          "title": {{- if .app.spec.source }} "Repository" {{- else if .app.spec.sources }} "Repositories" {{- end }},
          "value": {{- if .app.spec.source }} ":arrow_heading_up: {{ .app.spec.source.repoURL }}" ...
        },
        {"title": "Revision", "value": "{{.app.status.sync.revision}}", "short": true}
        {{range $index, $c := .app.status.conditions}},
        {"title": "{{$c.type}}", "value": "{{$c.message}}", "short": true}
        {{end}}
      ]
    }]
```

`app.spec.source` (단일 소스) vs `app.spec.sources` (다중 소스) 분기 처리가 포함되어 있다.

---

## 9. 아키텍처 흐름 상세

### 9.1 전체 처리 흐름

```
┌─────────────────────────────────────────────────────────────────┐
│ notifications-engine/pkg/controller                              │
│                                                                 │
│  Application 변경 (Create/Update) ──▶ Informer 이벤트          │
│                           │                                     │
│                           ▼                                     │
│              ┌────────────────────────┐                         │
│              │  WorkQueue에 Key 추가   │                         │
│              │  (namespace/name)       │                         │
│              └────────────┬───────────┘                         │
│                           │                                     │
│              ┌────────────▼───────────┐                         │
│              │  Worker (processors 수)│                         │
│              │                        │                         │
│              │  1. Application 조회   │                         │
│              │  2. Skip 조건 확인     │ ◀── SyncStatus 갱신 대기│
│              │  3. API 설정 로드      │ ◀── ConfigMap/Secret    │
│              └────────────┬───────────┘                         │
│                           │                                     │
│              ┌────────────▼───────────┐                         │
│              │  트리거 평가           │                         │
│              │  (expr 표현식 실행)    │                         │
│              └────────────┬───────────┘                         │
│                           │                                     │
│              ┌────────────▼───────────┐                         │
│              │  구독자 목록 조회      │                         │
│              │  - App annotation      │                         │
│              │  - AppProject annotation│                        │
│              │  - 글로벌 기본 구독    │                         │
│              └────────────┬───────────┘                         │
│                           │                                     │
│              ┌────────────▼───────────┐                         │
│              │  oncePer 중복 확인     │                         │
│              │  (annotation 기록 조회)│                         │
│              └────────────┬───────────┘                         │
│                           │                                     │
│              ┌────────────▼───────────┐                         │
│              │  템플릿 렌더링         │                         │
│              │  (Go template 실행)    │                         │
│              └────────────┬───────────┘                         │
│                           │                                     │
│              ┌────────────▼───────────┐                         │
│              │  서비스별 알림 전송    │                         │
│              │  Slack / Email / ...   │                         │
│              └────────────┬───────────┘                         │
│                           │                                     │
│              ┌────────────▼───────────┐                         │
│              │  Application annotation│                         │
│              │  에 전송 기록 업데이트 │                         │
│              └────────────────────────┘                         │
└─────────────────────────────────────────────────────────────────┘
```

### 9.2 Informer 기반 Application 변경 감지

```go
// notification_controller/controller/controller.go

func newInformer(
    resClient dynamic.ResourceInterface,
    controllerNamespace string,
    applicationNamespaces []string,
    selector string,
) cache.SharedIndexInformer {
    informer := cache.NewSharedIndexInformer(
        &cache.ListWatch{
            ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
                options.LabelSelector = selector
                appList, _ := resClient.List(ctx, options)

                // 허용된 네임스페이스의 Application만 필터링
                newItems := []unstructured.Unstructured{}
                for _, res := range appList.Items {
                    if controllerNamespace == res.GetNamespace() ||
                       glob.MatchStringInList(applicationNamespaces, res.GetNamespace(), glob.REGEXP) {
                        newItems = append(newItems, res)
                    }
                }
                appList.Items = newItems
                return appList, nil
            },
            WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
                options.LabelSelector = selector
                return resClient.Watch(ctx, options)
            },
        },
        &unstructured.Unstructured{},
        resyncPeriod, // 60초 주기 재동기화
        cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
    )
    return informer
}
```

`resyncPeriod = 60 * time.Second`: 60초마다 전체 Application 목록을 재처리하여 놓친 이벤트를 복구한다.

### 9.3 AppProject 조회 로직

```go
func getAppProj(
    app *unstructured.Unstructured,
    appProjInformer cache.SharedIndexInformer,
) *unstructured.Unstructured {
    // Application spec.project 필드에서 프로젝트 이름 추출
    projName, ok, _ := unstructured.NestedString(app.Object, "spec", "project")
    if !ok {
        return nil
    }

    // Informer 인메모리 캐시에서 AppProject 조회 (API 호출 없음)
    projObj, ok, _ := appProjInformer.GetIndexer().GetByKey(
        fmt.Sprintf("%s/%s", app.GetNamespace(), projName),
    )
    proj, _ := projObj.(*unstructured.Unstructured)

    // annotation nil 방지
    if proj.GetAnnotations() == nil {
        proj.SetAnnotations(map[string]string{})
    }
    return proj
}
```

---

## 10. 중복 방지 메커니즘

### 10.1 oncePer 필드

트리거의 `oncePer` 필드가 중복 방지의 핵심이다.

```yaml
trigger.on-sync-succeeded: |
  - when: app.status.operationState.phase in ['Succeeded']
    send: [app-sync-succeeded]
    oncePer: app.status.operationState?.syncResult?.revision
```

`oncePer`는 expr 표현식으로, 평가 결과 값이 이전에 알림을 전송했을 때의 값과 동일하면 전송을 건너뛴다.

예를 들어 `oncePer: app.status.operationState?.syncResult?.revision`이면:
- revision `abc123`으로 sync 성공 → 알림 전송, annotation에 `abc123` 기록
- 동일 revision `abc123`으로 재처리 → annotation의 `abc123`과 비교 → 전송 건너뜀
- revision `def456`으로 새 sync 성공 → 알림 전송, annotation에 `def456` 기록

### 10.2 annotation 기반 상태 기록

notifications-engine은 알림 전송 후 Application annotation에 전송 기록을 저장한다.

```yaml
# Application CR에 자동으로 추가되는 annotation (예시)
annotations:
  notified.notifications.argoproj.io: >-
    {
      "b64:b24tc3luYy1zdWNjZWVkZWQ6c2xhY2s6bXktY2hhbm5lbA==": {
        "b64:YWJjMTIz": 1234567890
      }
    }
```

이 annotation의 내용:
- 키: Base64 인코딩된 `trigger:service:recipient` 조합
- 값: `{oncePer 값: 타임스탬프}` 맵

### 10.3 SyncStatus 갱신 대기

알림 중복과는 다른 문제로, Operation 완료 시점에 SyncStatus가 아직 갱신되지 않은 경우 처리를 지연한다.

```
Operation 완료 (Succeeded)
        │
        ▼
isAppSyncStatusRefreshed 확인
        │
        ├─ finishedAt > reconciledAt  ──▶ 처리 스킵 (SyncStatus 미갱신)
        │                                  → 60초 후 재처리 시도
        │
        └─ finishedAt <= reconciledAt ──▶ 정상 처리 (SyncStatus 갱신 완료)
```

---

## 11. Self-Service 알림 모드

### 11.1 개요

`--self-service-notification-enabled` 플래그를 활성화하면, Application이 배포된 네임스페이스에서 직접 알림 설정을 관리할 수 있다.

```bash
# argocd-notifications-controller 실행 시
argocd-notifications --self-service-notification-enabled=true
```

### 11.2 Self-Service 모드 동작 차이

| 항목 | 일반 모드 | Self-Service 모드 |
|------|----------|-----------------|
| ConfigMap 감시 네임스페이스 | `argocd` 네임스페이스만 | 전체 네임스페이스 (`metav1.NamespaceAll`) |
| Secret 감시 네임스페이스 | `argocd` 네임스페이스만 | 전체 네임스페이스 |
| 컨트롤러 종류 | `NewController` | `NewControllerWithNamespaceSupport` |
| Secret 노출 | 없음 (Spawn 시 secrets 변수 불포함) | `initGetVarsWithoutSecret` 사용 |

Self-Service 모드에서는 각 네임스페이스에 `argocd-notifications-cm` ConfigMap을 만들어 해당 네임스페이스의 Application에만 적용되는 알림을 설정할 수 있다.

### 11.3 보안 고려사항

Self-Service 모드에서는 `initGetVarsWithoutSecret`을 사용하여 템플릿에서 `secrets` 변수에 접근할 수 없다. 이는 다른 네임스페이스의 민감한 Secret 데이터가 유출되는 것을 방지하기 위함이다.

```go
// util/notification/settings/settings.go

func initGetVarsWithoutSecret(
    argocdService service.Service,
    cfg *api.Config,
    configMap *corev1.ConfigMap,
    secret *corev1.Secret,
) (api.GetVars, error) {
    return func(obj map[string]any, dest services.Destination) map[string]any {
        return expression.Spawn(&unstructured.Unstructured{Object: obj}, argocdService, map[string]any{
            "app":     obj,
            "context": injectLegacyVar(context, dest.Service),
            // "secrets": secret.Data  ← 의도적으로 제외
        })
    }, nil
}
```

---

## 12. argocd CLI를 통한 알림 관리

### 12.1 CLI 명령어

`cmd/argocd/commands/admin/notifications.go`에서 관리 CLI를 제공한다.

```bash
# 트리거 목록 조회
argocd admin notifications trigger list

# 특정 Application에 대한 트리거 평가 테스트
argocd admin notifications trigger get on-sync-succeeded --app my-app

# 템플릿 목록 조회
argocd admin notifications template list

# 템플릿 렌더링 미리보기
argocd admin notifications template notify app-sync-succeeded --app my-app

# 서비스 목록 조회
argocd admin notifications service list
```

### 12.2 Notification Server API

`server/notification/notification.go`에서 gRPC API를 제공한다.

```go
type Server struct {
    apiFactory api.Factory
}

// 트리거 목록 반환
func (s *Server) ListTriggers(_ context.Context, _ *notification.TriggersListRequest) (*notification.TriggerList, error) {
    api, _ := s.apiFactory.GetAPI()
    triggers := []*notification.Trigger{}
    for trigger := range api.GetConfig().Triggers {
        triggers = append(triggers, &notification.Trigger{Name: new(trigger)})
    }
    return &notification.TriggerList{Items: triggers}, nil
}

// 서비스 목록 반환
func (s *Server) ListServices(_ context.Context, ...) (*notification.ServiceList, error) { ... }

// 템플릿 목록 반환
func (s *Server) ListTemplates(_ context.Context, ...) (*notification.TemplateList, error) { ... }
```

---

## 13. 레거시 설정 지원

### 13.1 레거시 형식

초기 Argo CD Notifications는 별도 standalone 프로젝트였다. 기존 설정 형식을 하위 호환성을 위해 지원한다.

```yaml
# 레거시 config.yaml 키 (현재는 deprecated)
config.yaml: |
  triggers:
  - name: on-sync-succeeded
    condition: app.status.operationState.phase in ['Succeeded']
    template: app-sync-succeeded
    enabled: true
  templates:
  - name: app-sync-succeeded
    body: Application {{.app.metadata.name}} has been synced.
  context:
    argocdUrl: https://argocd.company.com
```

```yaml
# 레거시 notifiers.yaml (현재는 deprecated)
notifiers.yaml: |
  slack:
    token: xoxb-xxx
  email:
    host: smtp.gmail.com
    port: 587
```

### 13.2 레거시 annotation 형식

```yaml
# 레거시 annotation (현재는 deprecated)
annotations:
  recipients.argocd-notifications.argoproj.io: slack:my-channel
  on-sync-succeeded.recipients.argocd-notifications.argoproj.io: slack:dev-channel
```

`GetLegacyDestinations` 함수가 이 형식을 파싱한다:

```go
// util/notification/settings/legacy.go
const annotationKey = "recipients.argocd-notifications.argoproj.io"

func GetLegacyDestinations(annotations map[string]string, ...) services.Destinations {
    for k, v := range annotations {
        if !strings.HasSuffix(k, annotationKey) {
            continue
        }
        triggerName := strings.TrimRight(k[0:len(k)-len(annotationKey)], ".")
        // ...recipient 파싱...
    }
}
```

---

## 14. ArgoCD Service: 커밋 메타데이터 접근

### 14.1 Service 인터페이스

```go
// util/notification/argocd/service.go

type Service interface {
    GetCommitMetadata(ctx context.Context, repoURL string, commitSHA string, project string) (*shared.CommitMetadata, error)
    GetAppDetails(ctx context.Context, app *v1alpha1.Application) (*shared.AppDetail, error)
}
```

### 14.2 GetCommitMetadata

repo-server를 통해 커밋 메시지, 작성자, 날짜, 태그를 조회한다.

```go
func (svc *argoCDService) GetCommitMetadata(
    ctx context.Context,
    repoURL string,
    commitSHA string,
    project string,
) (*shared.CommitMetadata, error) {
    argocdDB := db.NewDB(svc.namespace, svc.settingsMgr, svc.clientset)
    repo, _ := argocdDB.GetRepository(ctx, repoURL, project)

    metadata, _ := svc.repoServerClient.GetRevisionMetadata(ctx, &apiclient.RepoServerRevisionMetadataRequest{
        Repo:     repo,
        Revision: commitSHA,
    })

    return &shared.CommitMetadata{
        Message: metadata.Message,
        Author:  metadata.Author,
        Date:    metadata.Date.Time,
        Tags:    metadata.Tags,
    }, nil
}
```

템플릿에서 커밋 정보를 포함한 알림 예시:

```yaml
template.app-deployed: |
  message: |
    Application {{.app.metadata.name}} deployed commit
    {{with (call .repo.GetCommitMetadata .app.status.operationState.syncResult.revision)}}
    - Author: {{.Author}}
    - Message: {{.Message}}
    {{end}}
```

---

## 15. 왜(Why) 이런 설계인가

### 15.1 notifications-engine 라이브러리 분리

**문제**: 알림 시스템은 Argo Workflows, Rollouts 등 여러 Argo 프로젝트에서 공통으로 필요하다.

**해결**: `argoproj/notifications-engine`을 독립 라이브러리로 분리하여 여러 프로젝트에서 재사용한다. Argo CD는 이 라이브러리를 사용하는 클라이언트 중 하나이며, Application CR에 특화된 로직(AppProject 통합, SyncStatus 확인)만 추가한다.

```
notifications-engine (공통 라이브러리)
├── 트리거 평가 엔진
├── 템플릿 렌더링 엔진
├── 서비스 추상화 (Slack, Email, ...)
└── 구독 관리

Argo CD (notifications-engine 클라이언트)
├── Application Informer
├── AppProject 통합
├── SyncStatus 갱신 대기
└── ArgoCD 전용 표현식 함수 (repo.*)
```

### 15.2 Annotation 기반 구독

**문제**: 알림 구독 설정을 위해 별도 CRD를 만들거나 Application CR을 수정해야 한다.

**해결**: Kubernetes annotation을 구독 채널로 사용한다. 이로 인해:
- Argo CD CRD 스키마 변경 없이 구독 추가/제거 가능
- `kubectl annotate` 명령으로 즉시 적용 가능
- GitOps 방식으로 Application YAML에 annotation을 포함하여 선언적 관리 가능

### 15.3 ConfigMap 기반 설정 (Secret 분리)

**문제**: 알림 서비스 자격증명(토큰, 비밀번호)은 민감하지만, 트리거/템플릿은 민감하지 않다.

**해결**: ConfigMap과 Secret을 분리하여, 트리거/템플릿은 ConfigMap에, 자격증명은 Secret에 보관한다. ConfigMap에서 `$변수명`으로 Secret 값을 참조한다. 이로 인해:
- RBAC으로 ConfigMap과 Secret 접근 권한을 독립적으로 제어 가능
- 트리거/템플릿은 일반 개발자가 수정, 자격증명은 관리자만 수정

### 15.4 카탈로그 (notifications_catalog/)

**문제**: 모든 사용자가 트리거와 템플릿을 처음부터 작성하는 것은 번거롭다.

**해결**: 가장 일반적인 사용 사례(sync 성공/실패, health 저하, 배포 완료)를 카탈로그로 제공한다. 사용자는 `kubectl apply -f install.yaml` 하나로 즉시 사용 가능한 알림 시스템을 구성할 수 있다.

### 15.5 oncePer 기반 중복 방지

**문제**: 컨트롤러가 60초마다 전체 Application을 재처리하므로, 동일 이벤트에 대해 반복 알림이 전송될 수 있다.

**해결**: `oncePer` 표현식의 평가 결과를 Application annotation에 기록하여 동일 값에 대한 중복 전송을 방지한다. CRD나 외부 저장소 없이 Kubernetes annotation만으로 상태를 관리한다는 점이 간결하다.

### 15.6 SyncStatus 갱신 대기

**문제**: Application Controller가 Operation 완료 후 SyncStatus를 갱신하는 데 시간이 걸린다. Notification Controller가 이 갱신 전에 처리하면 잘못된 상태(예: OutOfSync)로 알림이 전송된다.

**해결**: `isAppSyncStatusRefreshed`가 `finishedAt`, `reconciledAt`, `observedAt` 타임스탬프를 비교하여 SyncStatus 갱신 완료를 확인한 후에만 알림 처리를 진행한다.

---

## 16. 메트릭

Notification Controller는 Prometheus 메트릭을 포트 `9001`로 노출한다.

| 메트릭 | 설명 |
|--------|------|
| `argocd_notifications_trigger_eval_total` | 트리거 평가 횟수 |
| `argocd_notifications_deliveries_total` | 알림 전송 횟수 (서비스별, 성공/실패) |
| `argocd_notifications_trigger_eval_duration_seconds` | 트리거 평가 소요 시간 |
| `argocd_notifications_queue_size` | 처리 대기 중인 Application 수 |

---

## 17. 트러블슈팅

### 17.1 알림이 전송되지 않는 경우

```bash
# 1. Notification Controller 로그 확인
kubectl logs -n argocd deployment/argocd-notifications-controller -f

# 2. Application annotation에 구독 확인
kubectl get app my-app -n argocd -o yaml | grep notifications

# 3. 트리거 평가 테스트
argocd admin notifications trigger get on-sync-succeeded --app my-app

# 4. 전송 기록 annotation 확인 (oncePer 중복 방지 작동 여부)
kubectl get app my-app -n argocd -o jsonpath='{.metadata.annotations.notified\.notifications\.argoproj\.io}'
```

### 17.2 ConfigMap 설정 확인

```bash
# 현재 적용된 ConfigMap 설정 확인
kubectl get configmap argocd-notifications-cm -n argocd -o yaml

# 트리거 목록
argocd admin notifications trigger list

# 서비스 목록
argocd admin notifications service list
```

### 17.3 알림 중복 전송 문제

`oncePer` 필드가 없거나 표현식이 항상 다른 값을 반환하는 경우 중복 전송이 발생한다. Application annotation의 `notified.notifications.argoproj.io`를 삭제하면 기록이 초기화된다.

```bash
kubectl annotate app my-app -n argocd notified.notifications.argoproj.io- --overwrite
```
