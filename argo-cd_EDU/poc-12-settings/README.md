# PoC 12: 설정 관리

## 개요

Argo CD의 설정 관리 시스템을 Go 표준 라이브러리만으로 시뮬레이션한다.
Argo CD는 `argocd-cm` ConfigMap과 `argocd-secret` Secret에서 모든 런타임 설정을 관리하며,
클러스터와 저장소 자격증명은 별도 K8s Secret으로 저장한다.

## 참조 소스 코드

| 파일 | 역할 |
|------|------|
| `util/settings/settings.go` | ArgoCDSettings, SettingsManager, Subscribe/Unsubscribe |
| `util/db/secrets.go` | URIToSecretName (FNV-32a 해시, 클러스터 시크릿 이름) |
| `util/db/repository.go` | RepoURLToSecretName (FNV-32a 해시, 저장소 시크릿 이름) |
| `common/common.go` | ArgoCDConfigMapName, ArgoCDSecretName, LabelKeySecretType |

## 핵심 개념

### ArgoCDSettings 구조

```go
// 실제 소스: util/settings/settings.go type ArgoCDSettings struct
type ArgoCDSettings struct {
    URL                         string        // 외부 URL (SSO 설정용)
    DexConfig                   string        // Dex YAML 설정
    OIDCConfigRAW               string        // OIDC YAML 설정
    ServerSignature             []byte        // JWT 서명 키
    TrackingMethod              string        // "annotation" | "label" | "annotation+label"
    ApplicationInstanceLabelKey string        // app 인스턴스 레이블 키
    StatusBadgeEnabled          bool
    AnonymousUserEnabled        bool
    UserSessionDuration         time.Duration
    WebhookGitHubSecret         string
    KustomizeBuildOptions       string
}
```

### SettingsManager 구조

```go
// 실제 소스: util/settings/settings.go type SettingsManager struct
type SettingsManager struct {
    ctx             context.Context
    clientset       kubernetes.Interface
    secrets         v1listers.SecretLister
    secretsInformer cache.SharedIndexInformer
    configmaps      v1listers.ConfigMapLister
    namespace       string
    subscribers     []chan<- *ArgoCDSettings  // 설정 변경 구독자
    mutex           *sync.Mutex
}
```

### ConfigMap 키 상수 (argocd-cm)

| 키 | 설명 |
|----|------|
| `url` | Argo CD 외부 URL |
| `dex.config` | Dex 설정 (YAML) |
| `oidc.config` | OIDC 설정 (YAML) |
| `application.resourceTrackingMethod` | 리소스 추적 방식 |
| `application.instanceLabelKey` | 앱 인스턴스 레이블 키 |
| `resource.customizations` | 리소스 타입별 커스터마이제이션 |
| `resource.exclusions` | 제외할 리소스 목록 |
| `resource.inclusions` | 포함할 리소스 목록 |
| `kustomize.buildOptions` | Kustomize 빌드 옵션 |
| `statusbadge.enabled` | 상태 뱃지 활성화 |
| `users.anonymous.enabled` | 익명 사용자 허용 |

### Secret 키 (argocd-secret)

| 키 | 설명 |
|----|------|
| `server.secretkey` | JWT 서명 키 (Base64) |
| `webhook.github.secret` | GitHub 웹훅 시크릿 |
| `webhook.gitlab.secret` | GitLab 웹훅 시크릿 |
| `webhook.bitbucket.uuid` | Bitbucket 웹훅 UUID |

### 설정 구독 패턴

```go
// 실제 소스: util/settings/settings.go
func (mgr *SettingsManager) Subscribe(subCh chan<- *ArgoCDSettings) {
    mgr.mutex.Lock()
    defer mgr.mutex.Unlock()
    mgr.subscribers = append(mgr.subscribers, subCh)
}

func (mgr *SettingsManager) notifySubscribers(newSettings *ArgoCDSettings) {
    // 데드락 방지를 위해 별도 고루틴에서 알림
    go func() {
        for _, sub := range subscribers {
            sub <- newSettings
        }
    }()
}
```

**구독자 예시:** Application Controller, API Server, Repo Server는 모두 설정 변경을 구독하여 실시간으로 설정을 반영한다.

### URIToSecretName (클러스터)

```go
// 실제 소스: util/db/secrets.go
func URIToSecretName(uriType, uri string) (string, error) {
    parsedURI, _ := url.ParseRequestURI(uri)
    host := parsedURI.Host  // 포트 제거, IPv6 정규화
    h := fnv.New32a()
    h.Write([]byte(uri))    // URI 전체 해시
    return fmt.Sprintf("%s-%s-%v", uriType, host, h.Sum32()), nil
}
```

**예시:**
```
URIToSecretName("cluster", "https://k8s.example.com:6443")
    → "cluster-k8s.example.com-1234567890"

URIToSecretName("cluster", "http://[fe80::1ff:fe23:4567:890a]:8000")
    → "cluster-fe80--1ff-fe23-4567-890a-664858999"
```

### RepoURLToSecretName (저장소)

```go
// 실제 소스: util/db/repository.go
// NOTE: this formula should not be considered stable and may change in future releases.
func RepoURLToSecretName(prefix string, repo string, project string) string {
    h := fnv.New32a()
    h.Write([]byte(repo))
    h.Write([]byte(project))  // project가 다르면 별도 Secret
    return fmt.Sprintf("%s-%v", prefix, h.Sum32())
}
```

**예시:**
```
RepoURLToSecretName("repo", "git@github.com:argoproj/argo-cd.git", "")
    → "repo-796709561"

RepoURLToSecretName("repo", "git@github.com:argoproj/argo-cd.git", "my-project")
    → "repo-747992871"  (동일 URL, 다른 project → 다른 Secret)
```

### K8s Secret 레이블

```go
// 실제 소스: common/common.go
const (
    LabelKeySecretType             = "argocd.argoproj.io/secret-type"
    LabelValueSecretTypeCluster    = "cluster"
    LabelValueSecretTypeRepository = "repository"
    LabelValueSecretTypeRepoCreds  = "repo-creds"
)
```

### 리소스 추적 방식

| 방식 | 값 | 동작 |
|------|-----|------|
| 어노테이션 (기본) | `annotation` | `argocd.argoproj.io/app` 어노테이션 사용 |
| 레이블 | `label` | `app.kubernetes.io/instance` 레이블 사용 |
| 어노테이션+레이블 | `annotation+label` | 둘 다 사용 |

```go
// 실제 소스: util/settings/settings.go
func (mgr *SettingsManager) GetTrackingMethod() (string, error) {
    tm := argoCDCM.Data[settingsResourceTrackingMethodKey]
    if tm == "" {
        return string(v1alpha1.TrackingMethodAnnotation), nil
    }
    return tm, nil
}
```

### 리소스 커스터마이제이션

`resource.customizations` ConfigMap 키로 리소스 타입별 동작을 오버라이드:

```yaml
resource.customizations: |
  apps/Deployment:
    health.lua: |
      hs = {}
      if obj.status.availableReplicas == obj.spec.replicas then
        hs.status = "Healthy"
      else
        hs.status = "Progressing"
      end
      return hs
  networking.k8s.io/Ingress:
    health.lua: |
      hs = {}
      hs.status = "Healthy"
      return hs
```

## 실행 방법

```bash
go run main.go
```

## 실행 결과 요약

```
ConfigMap 키 상수 표: 12개 ConfigMap 키 + 3개 Secret 키

SettingsManager 설정 로드:
  URL, TrackingMethod, ServerSignature 등 모든 필드 로드

설정 구독 패턴:
  AppController + APIServer 구독 등록 → 설정 변경 → 알림 수신 → 구독 해제

URIToSecretName (클러스터):
  in-cluster       → cluster-kubernetes.default.svc-3396314289
  us-west-2 :6443  → cluster-k8s-usw2.example.com-1853627128
  IPv6 클러스터    → cluster-fe80--1ff-fe23-4567-890a-664858999

RepoURLToSecretName (저장소):
  동일 URL, 다른 project → 다른 Hash (별도 Secret)
```

## 핵심 설계 선택의 이유 (Why)

**왜 클러스터/저장소를 별도 K8s Secret으로 저장하는가?**
ConfigMap은 암호화되지 않아 자격증명(비밀번호, SSH 키 등) 저장에 부적합하다. K8s Secret은 etcd 암호화, RBAC 접근 제어, 외부 시크릿 관리(Vault, AWS Secrets Manager 등)와 통합이 가능하다.

**왜 URIToSecretName이 FNV-32a 해시를 사용하는가?**
클러스터 URL은 특수문자(`://.`)를 포함하여 K8s Secret 이름(RFC 1123)으로 그대로 사용할 수 없다. FNV-32a는 속도가 빠르고 짧은 해시를 생성하므로 Secret 이름 길이 제한(253자)을 준수하면서도 충돌 가능성이 낮다. host를 prefix에 포함하여 디버깅 시 어떤 클러스터인지 파악하기 쉽다.

**왜 project가 다르면 같은 URL도 별도 Secret을 생성하는가?**
Argo CD는 멀티 테넌시를 지원하며, 같은 저장소에 프로젝트별로 다른 자격증명을 사용해야 하는 경우가 있다. project를 해시에 포함하여 프로젝트별 자격증명 격리를 실현한다.

**왜 설정 변경 알림을 별도 고루틴에서 수행하는가?**
`notifySubscribers`를 호출하는 시점에 이미 mutex를 보유한 상태일 수 있다. 구독자가 설정을 읽으려고 같은 mutex를 획득하려 하면 데드락이 발생한다. 별도 고루틴으로 알림을 보내 이를 방지한다.
