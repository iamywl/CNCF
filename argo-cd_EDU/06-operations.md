# Argo CD 운영 가이드

## 목차

1. [배포 방식](#1-배포-방식)
2. [핵심 설정](#2-핵심-설정)
3. [RBAC 정책](#3-rbac-정책)
4. [SSO / OIDC](#4-sso--oidc)
5. [모니터링](#5-모니터링)
6. [트러블슈팅](#6-트러블슈팅)
7. [성능 튜닝](#7-성능-튜닝)
8. [보안](#8-보안)

---

## 1. 배포 방식

Argo CD는 목적에 따라 여러 가지 설치 방식을 지원한다. 모든 매니페스트는
소스 저장소 `manifests/` 디렉토리 아래에 Kustomize 기반으로 관리된다.

### 1.1 manifests/ 디렉토리 구조

```
manifests/
├── base/                          # 공통 베이스 리소스
│   ├── application-controller/    # StatefulSet 정의
│   ├── applicationset-controller/ # Deployment 정의
│   ├── config/                    # ConfigMap / Secret 정의
│   ├── dex/                       # Dex SSO Deployment
│   ├── redis/                     # Redis Deployment
│   ├── repo-server/               # Repo Server Deployment
│   ├── server/                    # Argo CD Server Deployment
│   └── notification/              # 알림 컨트롤러
├── cluster-install/               # Cluster 범위 설치 (ClusterRole 포함)
├── namespace-install/             # Namespace 범위 설치
├── ha/                            # HA 구성 오버레이
│   ├── base/                      # HA 공통 베이스
│   ├── cluster-install/           # HA + ClusterRole
│   └── namespace-install/         # HA + Namespace 범위
├── core-install/                  # 경량 Core 설치
├── crds/                          # CRD 정의만
├── install.yaml                   # 일반 설치 (단일 파일)
├── namespace-install.yaml         # Namespace 범위 설치 (단일 파일)
├── cluster-install.yaml           # Cluster 범위 설치 (단일 파일)
└── core-install.yaml              # Core 설치 (단일 파일)
```

### 1.2 Namespace Install vs Cluster Install

두 설치 방식의 차이는 **RBAC 범위**에 있다.

| 항목 | Namespace Install | Cluster Install |
|------|-------------------|-----------------|
| ClusterRole | 없음 (Role만 사용) | 있음 |
| 대상 클러스터 | argocd 네임스페이스 한정 | 클러스터 전체 |
| 용도 | 단일 팀, 권한 제한 환경 | 전사 공용 GitOps |
| 외부 클러스터 관리 | 별도 ServiceAccount 필요 | 기본 지원 |
| 설치 명령 | `kubectl apply -n argocd -f namespace-install.yaml` | `kubectl apply -n argocd -f install.yaml` |

**Namespace Install** 은 Argo CD가 관리하는 리소스를 특정 네임스페이스에 한정하고 싶을 때 사용한다.
이 경우 Argo CD는 다른 클러스터나 다른 네임스페이스를 관리하려면 명시적인 권한 부여가 필요하다.

**Cluster Install** 은 ClusterRole을 사용하므로 클러스터 전체 리소스를 조회/관리할 수 있다.
기업 환경에서 중앙 GitOps 플랫폼을 운영할 때 주로 선택한다.

### 1.3 Core Install (경량 설치)

`core-install.yaml` 은 UI와 API 서버 없이 핵심 컨트롤러만 설치하는 방식이다.

```
포함:
  - application-controller (StatefulSet)
  - applicationset-controller (Deployment)
  - redis (Deployment)
  - repo-server (Deployment)

미포함:
  - argocd-server (UI/API)
  - dex (SSO)
  - notification 컨트롤러
```

Core Install에서는 `argocd` CLI를 직접 kubectl과 함께 사용하며,
`argocd app get <app>` 대신 `kubectl get application <app> -n argocd` 를 사용한다.

### 1.4 HA (High Availability) 구성

`manifests/ha/` 디렉토리는 프로덕션 HA 환경을 위한 Kustomize 오버레이를 제공한다.

```yaml
# manifests/ha/base/kustomization.yaml 에서 참조하는 오버레이들
patches:
  - path: overlays/argocd-repo-server-deployment.yaml    # 레플리카 수 증가
  - path: overlays/argocd-server-deployment.yaml         # 레플리카 수 증가
  - path: overlays/argocd-application-controller-statefulset.yaml  # 샤딩 설정
  - path: overlays/argocd-cmd-params-cm.yaml             # 파라미터 조정

resources:
  - ../../base/application-controller
  - ../../base/applicationset-controller
  - ../../base/dex
  - ../../base/repo-server
  - ../../base/server
  - ../../base/config
  - ../../base/notification
  - ./redis-ha    # Redis HA (Sentinel 모드)
```

HA 구성의 핵심 포인트:

| 컴포넌트 | 단일 인스턴스 | HA 구성 |
|----------|--------------|---------|
| argocd-server | 1 | 2+ (Deployment) |
| repo-server | 1 | 2+ (Deployment) |
| application-controller | 1 | 샤딩 + 다중 인스턴스 (StatefulSet) |
| redis | 단독 | Redis HA (Sentinel) |
| dex | 1 | 1 (무상태) |

**Redis HA** 는 `redis-ha/` 서브디렉토리에서 Redis Sentinel 기반 구성을 제공한다.
Sentinel은 마스터 장애 시 자동으로 슬레이브를 마스터로 승격시킨다.

### 1.5 Helm Chart 배포

Argo CD 공식 Helm 차트는 `argo/argo-cd` 저장소에서 관리된다.

```bash
# 저장소 추가
helm repo add argo https://argoproj.github.io/argo-helm
helm repo update

# 기본 설치
helm install argocd argo/argo-cd \
  --namespace argocd \
  --create-namespace

# values.yaml을 사용한 커스텀 설치
helm install argocd argo/argo-cd \
  --namespace argocd \
  --create-namespace \
  -f values.yaml

# HA 설치
helm install argocd argo/argo-cd \
  --namespace argocd \
  --create-namespace \
  --set controller.replicas=2 \
  --set server.replicas=2 \
  --set repoServer.replicas=2 \
  --set redis-ha.enabled=true \
  --set redis.enabled=false
```

Helm values의 주요 구조:

```yaml
# values.yaml 주요 항목
global:
  image:
    tag: v2.x.x

controller:
  replicas: 1                      # application-controller 인스턴스 수
  env:
    - name: ARGOCD_CONTROLLER_REPLICAS
      value: "1"

server:
  replicas: 1
  ingress:
    enabled: true
    hostname: argocd.example.com

repoServer:
  replicas: 1
  parallelismLimit: 10             # 동시 매니페스트 생성 제한

configs:
  cm:
    url: "https://argocd.example.com"
    dex.config: |
      connectors:
        - type: github
          ...
  rbac:
    policy.csv: |
      p, role:org-admin, applications, *, */*, allow
    policy.default: role:readonly

redis-ha:
  enabled: false                   # HA Redis 활성화 시 true
```

---

## 2. 핵심 설정

Argo CD의 모든 설정은 Kubernetes ConfigMap과 Secret으로 관리된다.
이 방식을 통해 Argo CD 자체도 GitOps 방식으로 설정을 버전 관리할 수 있다.

### 2.1 argocd-cm ConfigMap

`argocd-cm` 은 Argo CD의 핵심 동작을 제어하는 주 설정 ConfigMap이다.
소스 위치: `manifests/base/config/argocd-cm.yaml`

#### URL 설정

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-cm
  namespace: argocd
data:
  # 외부 접근 URL — SSO 콜백, 알림 링크에 사용
  url: "https://argocd.example.com"
```

이 값이 설정되지 않으면 SSO 리다이렉트와 알림의 링크가 정상 동작하지 않는다.

#### Dex SSO 설정 (dex.config)

소스 코드에서 설정 키 확인 (`util/settings/settings.go:439`):
```go
settingDexConfigKey = "dex.config"
```

```yaml
data:
  dex.config: |
    connectors:
      # GitHub OAuth 연동
      - type: github
        id: github
        name: GitHub
        config:
          clientID: $dex.github.clientID        # argocd-secret에서 참조
          clientSecret: $dex.github.clientSecret
          orgs:
            - name: my-github-org
              teams:
                - platform-team

      # LDAP 연동
      - type: ldap
        id: ldap
        name: LDAP
        config:
          host: ldap.example.com:636
          insecureNoSSL: false
          bindDN: cn=admin,dc=example,dc=com
          bindPW: $dex.ldap.bindPW
          userSearch:
            baseDN: ou=users,dc=example,dc=com
            filter: "(objectClass=person)"
            username: uid
            idAttr: uid
            emailAttr: mail
            nameAttr: cn
          groupSearch:
            baseDN: ou=groups,dc=example,dc=com
            filter: "(objectClass=groupOfNames)"
            userAttr: DN
            groupAttr: member
            nameAttr: cn

      # SAML 연동
      - type: saml
        id: saml
        name: SAML
        config:
          ssoURL: https://idp.example.com/sso
          caData: <base64-encoded-ca>
          redirectURI: https://argocd.example.com/api/dex/callback
          usernameAttr: email
          emailAttr: email
          groupsAttr: groups
```

#### 외부 OIDC 설정 (oidc.config)

소스 코드에서 설정 키 확인 (`util/settings/settings.go:441`):
```go
settingsOIDCConfigKey = "oidc.config"
```

Dex를 사용하지 않고 외부 OIDC 프로바이더(Okta, Azure AD, Google Workspace 등)와 직접 연동할 때 사용한다.

```yaml
data:
  oidc.config: |
    name: Azure AD
    issuer: https://login.microsoftonline.com/{tenant}/v2.0
    clientID: <azure-app-client-id>
    clientSecret: $oidc.azure.clientSecret
    requestedScopes:
      - openid
      - profile
      - email
      - groups
    requestedIDTokenClaims:
      groups:
        essential: true
    # 사용자 정보 캐시 만료 (기본값 없음)
    userInfoCacheExpiration: "5m"
```

#### 앱 추적 설정

소스 코드에서 설정 키 확인 (`util/settings/settings.go:463,465`):
```go
settingsApplicationInstanceLabelKey = "application.instanceLabelKey"
settingsResourceTrackingMethodKey   = "application.resourceTrackingMethod"
```

```yaml
data:
  # 어떤 레이블로 앱이 소유한 리소스를 추적할지 지정
  # 기본값: app.kubernetes.io/instance
  application.instanceLabelKey: app.kubernetes.io/instance

  # 리소스 추적 방식 (label / annotation / annotation+label)
  # label: 레이블만 사용 (기본값, 하위 호환)
  # annotation: argocd.argoproj.io/tracking-id 어노테이션 사용
  # annotation+label: 둘 다 사용
  application.resourceTrackingMethod: annotation
```

`annotation` 방식을 쓰면 여러 Argo CD 인스턴스가 같은 클러스터를 관리할 때
레이블 충돌을 방지할 수 있다.

#### 리소스 커스터마이징 (resource.customizations)

소스 코드에서 설정 키 확인 (`util/settings/settings.go:471`):
```go
resourceCustomizationsKey = "resource.customizations"
```

Lua 스크립트로 리소스의 Health 상태를 커스텀 정의하거나, 액션을 추가할 수 있다.

```yaml
data:
  # Health 커스터마이징: cert-manager Certificate 리소스
  resource.customizations.health.cert-manager.io_Certificate: |
    hs = {}
    if obj.status ~= nil then
      if obj.status.conditions ~= nil then
        for i, condition in ipairs(obj.status.conditions) do
          if condition.type == "Ready" and condition.status == "False" then
            hs.status = "Degraded"
            hs.message = condition.message
            return hs
          end
          if condition.type == "Ready" and condition.status == "True" then
            hs.status = "Healthy"
            hs.message = condition.message
            return hs
          end
        end
      end
    end
    hs.status = "Progressing"
    hs.message = "Waiting for certificate"
    return hs

  # Action 커스터마이징: Deployment 재시작 액션 추가
  resource.customizations.actions.apps_Deployment: |
    discovery.lua: |
      actions = {}
      actions["restart"] = {}
      return actions
    definitions:
    - name: restart
      action.lua: |
        local os = require("os")
        if obj.spec.template.metadata == nil then
            obj.spec.template.metadata = {}
        end
        if obj.spec.template.metadata.annotations == nil then
            obj.spec.template.metadata.annotations = {}
        end
        obj.spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"] = os.date("!%Y-%m-%dT%TZ")
        return obj

  # 기본 ignoreResourceUpdates: status 변경 무시 (소스에서 확인)
  resource.customizations.ignoreResourceUpdates.all: |
    jsonPointers:
      - /status
```

#### 리소스 필터링

소스 코드에서 설정 키 확인 (`util/settings/settings.go:473,475`):
```go
resourceExclusionsKey = "resource.exclusions"
resourceInclusionsKey = "resource.inclusions"
```

```yaml
data:
  # 리소스 제외 — 감시 대상에서 제외할 리소스 (성능 최적화)
  # argocd-cm.yaml 기본값에서 확인된 실제 설정
  resource.exclusions: |
    # 고빈도 변경 네트워크 리소스 제외
    - apiGroups:
      - ''
      - discovery.k8s.io
      kinds:
      - Endpoints
      - EndpointSlice
    # 리더 선출 리소스 제외
    - apiGroups:
      - coordination.k8s.io
      kinds:
      - Lease
    # Authz/Authn 리소스 제외
    - apiGroups:
      - authentication.k8s.io
      - authorization.k8s.io
      kinds:
      - '*'
    # 특정 네임스페이스의 모든 리소스 제외
    - apiGroups:
      - '*'
      kinds:
      - '*'
      namespaces:
      - kube-system

  # 리소스 포함 — exclusions보다 우선순위 낮음, 특정 리소스만 허용
  resource.inclusions: |
    - apiGroups:
      - '*'
      kinds:
      - '*'
      clusters:
      - https://my-cluster.example.com
```

#### Kustomize 빌드 옵션

소스 코드에서 설정 키 확인 (`util/settings/settings.go:487`):
```go
kustomizeBuildOptionsKey = "kustomize.buildOptions"
```

```yaml
data:
  # 모든 Kustomize 빌드에 적용할 공통 옵션
  kustomize.buildOptions: --enable-helm --load-restrictor=LoadRestrictionsNone

  # 특정 버전별 빌드 옵션
  kustomize.buildOptions.v4.5.7: --enable-helm
  kustomize.buildOptions.v5.0.0: --enable-helm --enable-alpha-plugins
```

#### 접근 제어 설정

소스 코드에서 설정 키 확인 (`util/settings/settings.go:493,531,539`):
```go
anonymousUserEnabledKey = "users.anonymous.enabled"
inClusterEnabledKey     = "cluster.inClusterEnabled"
execEnabledKey          = "exec.enabled"
```

```yaml
data:
  # 익명 접근 허용 여부 (기본값: false)
  # true로 설정 시 로그인 없이 읽기 전용 접근 허용
  users.anonymous.enabled: "false"

  # Pod exec 기능 활성화 (기본값: false)
  # UI/CLI에서 argocd app exec 명령 허용
  exec.enabled: "false"

  # in-cluster 접근 허용 여부 (기본값: true)
  # false로 설정 시 Argo CD가 자신이 실행 중인 클러스터를 관리하지 못하게 함
  cluster.inClusterEnabled: "true"
```

### 2.2 argocd-secret Secret

`argocd-secret` 은 민감한 정보를 저장하는 Kubernetes Secret이다.
소스 위치: `manifests/base/config/argocd-secret.yaml`

소스 코드에서 확인된 키들 (`util/settings/settings.go:447-453`):
```go
settingsWebhookGitHubSecretKey       = "webhook.github.secret"
settingsWebhookGitLabSecretKey       = "webhook.gitlab.secret"
settingsWebhookBitbucketUUIDKey      = "webhook.bitbucket.uuid"
settingsWebhookBitbucketServerSecretKey = "webhook.bitbucketserver.secret"
```

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: argocd-secret
  namespace: argocd
type: Opaque
stringData:
  # JWT 서명 키 — Argo CD 서버가 JWT 토큰 서명에 사용 (HMAC-SHA256)
  # 변경 시 모든 기존 세션이 무효화됨
  server.secretkey: "<32바이트-이상-무작위-문자열>"

  # 관리자 비밀번호 (bcrypt 해시)
  # argocd admin initial-password 명령으로 초기 비밀번호 확인
  admin.password: "$2a$10$..."
  admin.passwordMtime: "2024-01-01T00:00:00Z"

  # GitHub Webhook 시크릿
  # GitHub 저장소 Settings > Webhooks에서 설정한 시크릿과 동일
  webhook.github.secret: "<github-webhook-secret>"

  # GitLab Webhook 시크릿
  webhook.gitlab.secret: "<gitlab-webhook-secret>"

  # Bitbucket Webhook UUID
  webhook.bitbucket.uuid: "<bitbucket-uuid>"

  # Dex OAuth 클라이언트 시크릿 (Dex 사용 시)
  dex.github.clientSecret: "<github-oauth-app-secret>"

  # 외부 OIDC 클라이언트 시크릿
  oidc.azure.clientSecret: "<azure-app-client-secret>"
```

**server.secretkey 생성 방법:**
```bash
# 안전한 랜덤 키 생성
kubectl create secret generic argocd-secret \
  --from-literal=server.secretkey=$(openssl rand -base64 32) \
  -n argocd --dry-run=client -o yaml | kubectl apply -f -
```

### 2.3 argocd-rbac-cm ConfigMap

RBAC 정책을 정의하는 ConfigMap이다.
소스 코드에서 확인된 키들 (`util/rbac/rbac.go:36-43`):

```go
ConfigMapPolicyCSVKey     = "policy.csv"
ConfigMapPolicyDefaultKey = "policy.default"
ConfigMapScopesKey        = "scopes"
ConfigMapMatchModeKey     = "policy.matchMode"
GlobMatchMode             = "glob"
RegexMatchMode            = "regex"
```

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-rbac-cm
  namespace: argocd
data:
  # RBAC 매칭 모드: glob (기본값) 또는 regex
  policy.matchMode: "glob"

  # 인증되지 않은 사용자의 기본 역할
  policy.default: role:readonly

  # JWT 클레임에서 그룹 정보를 읽을 스코프
  scopes: "[groups, email]"

  # RBAC 정책 정의
  policy.csv: |
    # GitHub 팀 → Argo CD 역할 매핑
    g, my-org:platform-team, role:admin
    g, my-org:dev-team, role:developer

    # 커스텀 역할 정의
    p, role:developer, applications, get, */*, allow
    p, role:developer, applications, sync, my-project/*, allow
    p, role:developer, applications, create, my-project/*, allow
    p, role:developer, logs, get, my-project/*, allow
    p, role:developer, exec, create, my-project/*, deny

    # 특정 사용자에게 직접 권한 부여
    p, user@example.com, applications, *, production/*, allow
```

---

## 3. RBAC 정책

Argo CD의 RBAC는 [Casbin](https://casbin.org/) 라이브러리를 기반으로 구현된다.

### 3.1 정책 형식

소스 코드 (`assets/builtin-policy.csv`) 에서 확인된 실제 정책 형식:

```
# 프로젝트 범위 리소스 (applications, applicationsets, logs, exec)
p, <subject>, <resource>, <action>, <project>/<object>, <allow|deny>

# 글로벌 리소스 (clusters, repositories, projects, certificates, accounts, gpgkeys, extensions)
p, <subject>, <resource>, <action>, <object>, <allow|deny>

# 그룹/역할 매핑
g, <subject>, <role>
```

각 필드 설명:

| 필드 | 설명 | 예시 |
|------|------|------|
| subject | 사용자, 그룹, 또는 역할 | `user@example.com`, `my-org:team`, `role:admin` |
| resource | 대상 리소스 종류 | `applications`, `clusters`, `repositories` |
| action | 수행할 액션 | `get`, `create`, `update`, `delete`, `sync`, `override` |
| project/object | 프로젝트와 대상 (프로젝트 범위 리소스만) | `my-project/*`, `*/my-app` |
| object | 대상 (글로벌 리소스) | `*`, `my-cluster` |
| effect | 허용 또는 거부 | `allow`, `deny` |

### 3.2 빌트인 역할

소스 코드 (`assets/builtin-policy.csv`) 에서 확인된 실제 빌트인 정책:

**role:readonly** — 모든 리소스에 대한 읽기 전용 권한:

```csv
p, role:readonly, applications, get, */*, allow
p, role:readonly, applicationsets, get, */*, allow
p, role:readonly, certificates, get, *, allow
p, role:readonly, clusters, get, *, allow
p, role:readonly, repositories, get, *, allow
p, role:readonly, write-repositories, get, *, allow
p, role:readonly, projects, get, *, allow
p, role:readonly, accounts, get, *, allow
p, role:readonly, gpgkeys, get, *, allow
p, role:readonly, logs, get, */*, allow
```

**role:admin** — 모든 리소스에 대한 전체 권한:

```csv
p, role:admin, applications, create, */*, allow
p, role:admin, applications, update, */*, allow
p, role:admin, applications, delete, */*, allow
p, role:admin, applications, sync, */*, allow
p, role:admin, applications, override, */*, allow
p, role:admin, applications, action/*, */*, allow
p, role:admin, applicationsets, get, */*, allow
p, role:admin, applicationsets, create, */*, allow
p, role:admin, applicationsets, update, */*, allow
p, role:admin, applicationsets, delete, */*, allow
p, role:admin, certificates, create, *, allow
p, role:admin, clusters, create, *, allow
# ... (전체 내용은 assets/builtin-policy.csv 참조)

# role:admin은 role:readonly를 상속
g, role:admin, role:readonly
# admin 사용자는 role:admin에 할당
g, admin, role:admin
```

### 3.3 리소스 범위

소스 코드 (`util/rbac/rbac.go:58-80`) 에서 확인된 리소스 상수들:

```go
ResourceClusters          = "clusters"
ResourceProjects          = "projects"
ResourceApplications      = "applications"
ResourceApplicationSets   = "applicationsets"
ResourceRepositories      = "repositories"
ResourceWriteRepositories = "write-repositories"
ResourceCertificates      = "certificates"
ResourceAccounts          = "accounts"
ResourceGPGKeys           = "gpgkeys"
ResourceLogs              = "logs"
ResourceExec              = "exec"
ResourceExtensions        = "extensions"
```

**프로젝트 범위** (project/object 형식): `applications`, `applicationsets`, `logs`, `exec`

**글로벌 범위** (object 형식): `clusters`, `repositories`, `write-repositories`, `projects`, `certificates`, `accounts`, `gpgkeys`, `extensions`

### 3.4 액션 종류

소스 코드 (`util/rbac/rbac.go:73-80`) 에서 확인된 액션 상수들:

```go
ActionGet      = "get"
ActionCreate   = "create"
ActionUpdate   = "update"
ActionDelete   = "delete"
ActionSync     = "sync"
ActionOverride = "override"
ActionAction   = "action"
```

### 3.5 RBAC 정책 예시

#### 프로젝트별 팀 분리

```csv
# 개발팀: 특정 프로젝트에 대한 sync 권한
p, role:dev-team, applications, get, dev-project/*, allow
p, role:dev-team, applications, sync, dev-project/*, allow
p, role:dev-team, logs, get, dev-project/*, allow

# 운영팀: production 프로젝트 전체 권한
p, role:ops-team, applications, *, production/*, allow
p, role:ops-team, clusters, get, *, allow

# CI 서비스 계정: 특정 앱만 sync
p, role:ci-bot, applications, sync, */my-app, allow
p, role:ci-bot, applications, get, */my-app, allow

# 그룹 매핑 (OIDC groups claim)
g, "engineering@example.com", role:dev-team
g, "operations@example.com", role:ops-team
g, "ci-service-account", role:ci-bot
```

#### Glob vs Regex 매칭

```yaml
# Glob 매칭 (기본값) — 와일드카드(*, ?) 사용
policy.matchMode: "glob"
policy.csv: |
  p, user@example.com, applications, get, */*, allow
  p, user@example.com, applications, sync, prod-*/*, allow

# Regex 매칭 — 정규식 사용
policy.matchMode: "regex"
policy.csv: |
  p, user@example.com, applications, get, .*/.*,  allow
  p, user@example.com, applications, sync, prod-.*/.*,  allow
```

### 3.6 RBAC 검증

`argocd admin settings rbac` 명령으로 정책을 적용 전에 검증할 수 있다:

```bash
# 정책 파일 검증
argocd admin settings rbac validate \
  --policy-file policy.csv \
  --namespace argocd

# 특정 권한 확인 (건식 실행)
argocd admin settings rbac can \
  user@example.com applications sync "my-project/my-app" \
  --policy-file policy.csv \
  --namespace argocd
```

---

## 4. SSO / OIDC

### 4.1 Dex 통합

Dex는 Argo CD에 내장된 OpenID Connect ID 프로바이더로, 다양한 인증 백엔드를 통합하는 역할을 한다.

```
인증 흐름:
사용자 → Argo CD Server → Dex → 외부 IdP (GitHub/LDAP/SAML)
                      ←── ID Token ──────────────────────
```

Dex가 지원하는 커넥터 종류:

| 커넥터 | 용도 |
|--------|------|
| github | GitHub OAuth App |
| gitlab | GitLab OAuth |
| google | Google OAuth 2.0 |
| microsoft | Azure AD (v1) |
| oidc | 표준 OIDC 프로바이더 |
| ldap | LDAP/Active Directory |
| saml | SAML 2.0 |
| authproxy | 리버스 프록시 인증 |

#### Dex GitHub 연동 예시

```yaml
# argocd-cm
dex.config: |
  connectors:
    - type: github
      id: github
      name: GitHub
      config:
        # GitHub OAuth App의 Client ID/Secret
        # argocd-secret에서 변수 참조 ($) 사용
        clientID: $dex.github.clientID
        clientSecret: $dex.github.clientSecret

        # 조직 기반 접근 제어
        orgs:
          - name: my-company        # GitHub 조직명

        # 팀 클레임 포함 여부
        loadAllGroups: false
        teamNameField: slug         # both, slug, name 중 선택
        useLoginAsID: false

# argocd-secret
stringData:
  dex.github.clientID: "<github-oauth-app-client-id>"
  dex.github.clientSecret: "<github-oauth-app-client-secret>"
```

```yaml
# argocd-rbac-cm — GitHub 팀 기반 RBAC
data:
  scopes: "[groups]"
  policy.csv: |
    # GitHub 팀 이름 형식: org:team-name
    g, my-company:platform-engineers, role:admin
    g, my-company:developers, role:developer
```

#### Dex Google 연동 예시

```yaml
dex.config: |
  connectors:
    - type: google
      id: google
      name: Google
      config:
        clientID: $dex.google.clientID
        clientSecret: $dex.google.clientSecret
        redirectURI: https://argocd.example.com/api/dex/callback
        # Google Groups 조회 (서비스 계정 필요)
        serviceAccountFilePath: /tmp/google-sa-key.json
        adminEmail: admin@example.com
        groups:
          - platform-team@example.com
```

### 4.2 외부 OIDC 프로바이더 직접 연동

Dex 없이 외부 OIDC 프로바이더와 직접 연동하려면 `oidc.config` 를 사용한다.

#### Azure AD 연동

```yaml
data:
  oidc.config: |
    name: Azure AD
    issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
    clientID: <azure-app-client-id>
    clientSecret: $oidc.azure.clientSecret
    requestedScopes:
      - openid
      - profile
      - email
      - "https://graph.microsoft.com/GroupMember.Read.All"
    requestedIDTokenClaims:
      groups:
        essential: true
    # 그룹을 Object ID 대신 이름으로 사용
    enablePKCEAuthentication: true
```

#### Okta 연동

```yaml
data:
  oidc.config: |
    name: Okta
    issuer: https://my-org.okta.com
    clientID: <okta-app-client-id>
    clientSecret: $oidc.okta.clientSecret
    requestedScopes:
      - openid
      - profile
      - email
      - groups
    requestedIDTokenClaims:
      groups:
        essential: true
```

### 4.3 JWT 토큰 관리

소스 코드 (`util/session/sessionmanager.go`) 에서 확인된 실제 구현:

```go
// JWT 서명 알고리즘: HMAC-SHA256 (HS256)
token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

// 토큰 자동 갱신 임계값: 만료 5분 전
autoRegenerateTokenDuration = time.Minute * 5

// 서명 검증
token, err := jwt.ParseWithClaims(tokenString, &claims, func(token *jwt.Token) (any, error) {
    if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
        return nil, status.Errorf(codes.Unauthenticated, ...)
    }
    ...
})

// 토큰 갱신 조건 (sessionmanager.go:305)
if remainingDuration < autoRegenerateTokenDuration && capability == settings.AccountCapabilityLogin {
    // 새 토큰 발급
}
```

토큰 관리 요약:

| 항목 | 값 |
|------|-----|
| 서명 알고리즘 | HMAC-SHA256 (HS256) |
| 서명 키 | `argocd-secret`의 `server.secretkey` |
| 자동 갱신 시점 | 만료 5분 전 |
| 세션 무효화 | `server.secretkey` 변경 시 전체 무효화 |

---

## 5. 모니터링

### 5.1 Prometheus 메트릭

각 컴포넌트는 `/metrics` 엔드포인트를 통해 Prometheus 메트릭을 노출한다.

| 컴포넌트 | 기본 포트 |
|----------|-----------|
| argocd-server | 8083 |
| application-controller | 8082 |
| repo-server | 8084 |
| applicationset-controller | 8080 |

#### application-controller 핵심 메트릭

소스 위치: `controller/metrics/metrics.go`

```
argocd_app_info
  - 설명: 애플리케이션 정보 (게이지)
  - 레이블: namespace, name, project, autosync_enabled, repo, dest_server,
            dest_namespace, sync_status, health_status, operation
  - 용도: 앱 상태 현황판, 상태별 집계

argocd_app_sync_total
  - 설명: 동기화 횟수 (카운터)
  - 레이블: namespace, name, project, dest_server, phase, dry_run
  - phase 값: Succeeded, Failed, Error

argocd_app_reconcile
  - 설명: 조정(reconcile) 소요 시간 (히스토그램, 초 단위)
  - 레이블: namespace, dest_server
  - 버킷: 0.25, 0.5, 1, 2, 4, 8, 16 초
  - 코드: Buckets: []float64{0.25, .5, 1, 2, 4, 8, 16}

argocd_app_sync_duration_seconds_total
  - 설명: 동기화 소요 시간 합계 (카운터)
  - 레이블: namespace, name, project, dest_server

argocd_app_k8s_request_total
  - 설명: Kubernetes API 요청 횟수
  - 레이블: namespace, name, project, server, response_code, verb,
            resource_kind, resource_namespace, dry_run

argocd_kubectl_exec_total
  - 설명: kubectl 실행 횟수
  - 레이블: hostname, command

argocd_kubectl_exec_pending
  - 설명: 대기 중인 kubectl 실행 수 (게이지)
  - 레이블: hostname, command

argocd_cluster_events_total
  - 설명: 처리된 Kubernetes 리소스 이벤트 수
  - 레이블: server, namespace, name, group, kind
```

#### repo-server 핵심 메트릭

소스 위치: `reposerver/metrics/metrics.go`

```
argocd_repo_pending_request_total
  - 설명: 레포지토리 락을 기다리는 요청 수 (게이지)
  - 레이블: repo
  - 의미: 이 값이 지속적으로 높으면 repo-server 병목

argocd_git_fetch_fail_total
  - 설명: git fetch 실패 횟수
  - 레이블: repo, revision

argocd_git_lsremote_fail_total
  - 설명: git ls-remote 실패 횟수
  - 레이블: repo, revision

argocd_git_request_total
  - 설명: git 요청 횟수
  - 레이블: (git 요청 유형별)

argocd_redis_request_total
  - 설명: Redis 요청 횟수
  - 레이블: initiator, failed

argocd_redis_request_duration_seconds
  - 설명: Redis 요청 소요 시간 (히스토그램)
  - 버킷: 0.1, 0.25, 0.5, 1, 2 초
```

#### cluster-level 메트릭

소스 위치: `controller/metrics/clustercollector.go`

```
argocd_cluster_api_resource_objects
  - 설명: 각 클러스터의 캐시된 Kubernetes 리소스 오브젝트 수
  - 레이블: server, namespace, name

argocd_cluster_api_resources
  - 설명: 각 클러스터의 API 리소스 그룹 수
  - 레이블: server, namespace, name
```

### 5.2 Prometheus ServiceMonitor 설정

```yaml
# ServiceMonitor for application-controller
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: argocd-metrics
  namespace: argocd
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: argocd-metrics
  endpoints:
    - port: metrics
      interval: 30s
      path: /metrics

---
# ServiceMonitor for argocd-server
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: argocd-server-metrics
  namespace: argocd
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: argocd-server-metrics
  endpoints:
    - port: metrics
      interval: 30s
```

### 5.3 핵심 Prometheus 쿼리

```promql
# 동기화 실패율 (5분 평균)
rate(argocd_app_sync_total{phase="Failed"}[5m])

# 앱별 상태 현황
argocd_app_info{sync_status="OutOfSync"}

# 조정 시간 P99
histogram_quantile(0.99, rate(argocd_app_reconcile_bucket[5m]))

# repo-server 병목 감지
argocd_repo_pending_request_total > 5

# Redis 요청 지연 P95
histogram_quantile(0.95, rate(argocd_redis_request_duration_seconds_bucket[5m]))
```

### 5.4 Grafana 대시보드

Argo CD 공식 Grafana 대시보드 ID: **14584** (Argo CD) 와 **19974** (Argo CD v2)

주요 패널:
- Applications 상태 분포 (Synced/OutOfSync/Unknown)
- Health 상태 분포 (Healthy/Degraded/Progressing)
- Sync 성공/실패율 시계열
- Reconcile 시간 분포 (히스토그램)
- Repo Server 대기 요청 수
- Kubernetes API 요청 속도

### 5.5 OpenTelemetry Tracing

소스 코드 (`util/trace/trace.go`) 에서 확인된 실제 구현:

```go
// OTLP gRPC exporter 사용
func InitTracer(ctx context.Context, serviceName, otlpAddress string,
    otlpInsecure bool, otlpHeaders map[string]string, otlpAttrs []string) (func(), error) {
    // AlwaysSample: 모든 요청 추적
    provider := sdktrace.NewTracerProvider(
        sdktrace.WithSampler(sdktrace.AlwaysSample()),
        sdktrace.WithResource(res),
        sdktrace.WithSpanProcessor(bsp),
    )
    otel.SetTextMapPropagator(propagation.TraceContext{})
    otel.SetTracerProvider(provider)
}
```

OTLP 설정 방법 (소스 코드 `cmd/argocd-server/commands/argocd_server.go:315-318`에서 확인):

```bash
# 환경 변수로 설정
ARGOCD_SERVER_OTLP_ADDRESS=otel-collector:4317
ARGOCD_SERVER_OTLP_INSECURE=true
ARGOCD_SERVER_OTLP_HEADERS=Authorization=Bearer token

# 또는 argocd-server 실행 옵션
argocd-server \
  --otlp-address=otel-collector:4317 \
  --otlp-insecure=true \
  --otlp-headers="key1=value1,key2=value2" \
  --otlp-attrs="env:production,region:us-west-2"
```

동일한 OTLP 설정이 `argocd-application-controller`, `argocd-repo-server` 에도 지원된다.

---

## 6. 트러블슈팅

### 6.1 App Status 디버깅

#### OutOfSync 원인 파악

```bash
# 앱 상세 상태 확인 — ComparedTo, Diff 정보 포함
argocd app get my-app --hard-refresh

# 실제 diff 확인
argocd app diff my-app

# 리소스별 상태 확인
argocd app resources my-app

# 특정 리소스의 diff
argocd app diff my-app --resource "apps:Deployment:my-deployment"
```

OutOfSync 원인 유형:

| 원인 | 진단 방법 |
|------|----------|
| Git 변경 | `argocd app diff`로 실제 차이 확인 |
| 런타임 변경 | `kubectl describe` vs Git 매니페스트 비교 |
| ignoreDifferences 미설정 | 불필요한 필드가 diff에 포함되는지 확인 |
| 리소스 추적 방식 불일치 | `application.resourceTrackingMethod` 확인 |
| 레이블 충돌 | 다른 도구가 레이블 수정하는지 확인 |

#### ComparedTo 필드 분석

```bash
# kubectl로 Application CR 직접 확인
kubectl get application my-app -n argocd -o yaml | grep -A 10 comparedTo
```

```yaml
status:
  sync:
    status: OutOfSync
    comparedTo:
      source:
        repoURL: https://github.com/my-org/my-repo
        targetRevision: main
        path: k8s/
      destination:
        server: https://kubernetes.default.svc
        namespace: production
```

#### Degraded 원인 파악

```bash
# Health 상태 상세 확인
argocd app get my-app
argocd app resources my-app --health

# 특정 리소스 이벤트 확인
kubectl describe deployment my-deployment -n production
kubectl get events -n production --sort-by='.lastTimestamp'
```

Degraded 원인 유형:

| 원인 | 진단 방법 |
|------|----------|
| Pod 실행 실패 | `kubectl describe pod`, `kubectl logs` |
| PVC 마운트 실패 | PVC/PV 상태 확인 |
| Readiness Gate 실패 | Pod conditions 확인 |
| CrashLoopBackOff | 컨테이너 로그 확인 |
| 커스텀 Health 스크립트 오류 | Lua 스크립트 검증 |

### 6.2 Sync 실패 원인 분석

#### RBAC 원인

```bash
# 서버 로그에서 RBAC 거부 확인
kubectl logs -n argocd deployment/argocd-server | grep "permission denied"

# 특정 사용자 권한 확인
argocd admin settings rbac can user@example.com applications sync "my-project/my-app"
```

#### SyncWindow 원인

```bash
# 현재 활성 SyncWindow 확인
argocd proj windows list my-project

# SyncWindow 상태 확인 (강제 sync 가능 여부)
argocd app sync my-app --force   # SyncWindow 무시
```

```yaml
# AppProject의 SyncWindow 설정 예시
spec:
  syncWindows:
    - kind: deny
      schedule: "0 22 * * *"    # 매일 22:00
      duration: 8h               # 8시간 동안 sync 금지
      applications:
        - "*"
      manualSync: true           # 수동 sync는 허용
```

#### Validation 오류

```bash
# Sync 상태 메시지 확인
argocd app get my-app | grep "Sync Status"

# 건식 실행(Dry Run)으로 사전 검증
argocd app sync my-app --dry-run

# 매니페스트 생성 오류 확인 (repo-server 로그)
kubectl logs -n argocd deployment/argocd-repo-server | grep "error"
```

### 6.3 로그 레벨 설정

Argo CD 컴포넌트의 로그 레벨은 실행 인수 또는 환경 변수로 제어한다.

```bash
# argocd admin 명령으로 로그 형식/레벨 설정 (admin.go:85-86에서 확인)
argocd admin --logformat=json --loglevel=debug <subcommand>

# argocd-server 로그 레벨 변경
kubectl set env deployment/argocd-server -n argocd ARGOCD_LOG_LEVEL=debug

# 특정 컴포넌트 실시간 로그 확인
kubectl logs -f -n argocd deployment/argocd-server
kubectl logs -f -n argocd deployment/argocd-repo-server
kubectl logs -f -n argocd statefulset/argocd-application-controller
```

로그 레벨 종류: `debug`, `info` (기본값), `warn`, `error`

### 6.4 argocd admin 명령어

소스 코드 (`cmd/argocd/commands/admin/admin.go:73-83`) 에서 확인된 서브커맨드:

```bash
# cluster 관련 — 클러스터 샤딩 상태, 캐시 확인
argocd admin cluster stats
argocd admin cluster shards

# app 관련 — 앱 Health 체크, 매니페스트 생성
argocd admin app generate-spec my-app
argocd admin app diff-revision my-app HEAD

# settings 관련 — 설정 검증
argocd admin settings validate
argocd admin settings rbac validate --policy-file policy.csv
argocd admin settings rbac can user@example.com applications get '*/my-app'

# 백업/복원
argocd admin export > backup.yaml
argocd admin import < backup.yaml

# 초기 비밀번호 확인
argocd admin initial-password

# Redis 초기 비밀번호 확인
argocd admin redis-initial-password

# 알림 테스트
argocd admin notifications template get
argocd admin notifications trigger get

# 대시보드 (로컬 UI 열기)
argocd admin dashboard
```

### 6.5 일반적인 문제 해결

#### repo-server가 응답하지 않는 경우

```bash
# repo-server 로그 확인
kubectl logs -n argocd deployment/argocd-repo-server

# 레포지토리 상태 확인
argocd repo list

# 특정 레포지토리 연결 테스트
argocd repo get https://github.com/my-org/my-repo
```

#### 앱이 Unknown 상태인 경우

```bash
# application-controller 로그 확인
kubectl logs -n argocd statefulset/argocd-application-controller

# 앱 강제 조정
argocd app get my-app --refresh

# Redis 캐시 무효화 (Hard Refresh)
argocd app get my-app --hard-refresh
```

#### Redis 연결 오류

```bash
# Redis Pod 상태 확인
kubectl get pods -n argocd | grep redis

# Redis 직접 접속 테스트
kubectl exec -it -n argocd deployment/argocd-redis -- redis-cli ping

# Redis 메모리 사용량 확인
kubectl exec -it -n argocd deployment/argocd-redis -- redis-cli info memory
```

---

## 7. 성능 튜닝

### 7.1 application-controller 프로세서 조정

소스 코드 (`cmd/argocd-application-controller/commands/argocd_application_controller.go:263-264`) 에서 확인된 기본값:

```go
// status-processors: 기본값 20
command.Flags().IntVar(&statusProcessors, "status-processors",
    env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_STATUS_PROCESSORS", 20, 0, math.MaxInt32),
    "Number of application status processors")

// operation-processors: 기본값 10
command.Flags().IntVar(&operationProcessors, "operation-processors",
    env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_OPERATION_PROCESSORS", 10, 0, math.MaxInt32),
    "Number of application operation processors")

// kubectl-parallelism-limit: 기본값 20
command.Flags().Int64Var(&kubectlParallelismLimit, "kubectl-parallelism-limit",
    env.ParseInt64FromEnv("ARGOCD_APPLICATION_CONTROLLER_KUBECTL_PARALLELISM_LIMIT", 20, 0, math.MaxInt64),
    "Number of allowed concurrent kubectl fork/execs.")
```

조정 방법 (StatefulSet 환경변수):

```yaml
# argocd-application-controller StatefulSet 환경 변수
env:
  # 앱 상태 체크 병렬 처리 수 (앱 수가 많을수록 증가)
  - name: ARGOCD_APPLICATION_CONTROLLER_STATUS_PROCESSORS
    value: "50"

  # Sync 작업 병렬 처리 수 (동시 sync 요청이 많을수록 증가)
  - name: ARGOCD_APPLICATION_CONTROLLER_OPERATION_PROCESSORS
    value: "25"

  # kubectl 동시 실행 제한 (클러스터 API 서버 부하 고려)
  - name: ARGOCD_APPLICATION_CONTROLLER_KUBECTL_PARALLELISM_LIMIT
    value: "10"
```

권장 설정 기준:

| 앱 수 | status-processors | operation-processors |
|-------|------------------|---------------------|
| ~100 | 20 (기본값) | 10 (기본값) |
| ~500 | 50 | 25 |
| ~1000 | 100 | 50 |
| 1000+ | 클러스터 샤딩 고려 | 클러스터 샤딩 고려 |

### 7.2 repo-server parallelism-limit

소스 코드 (`cmd/argocd-repo-server/commands/argocd_repo_server.go:245`) 에서 확인된 설정:

```go
// parallelismlimit: 기본값 0 (제한 없음)
command.Flags().Int64Var(&parallelismLimit, "parallelismlimit",
    int64(env.ParseNumFromEnv("ARGOCD_REPO_SERVER_PARALLELISM_LIMIT", 0, 0, math.MaxInt32)),
    "Limit on number of concurrent manifests generate requests. Any value less the 1 means no limit.")
```

```yaml
# argocd-repo-server Deployment 환경 변수
env:
  # 동시 매니페스트 생성 요청 제한 (메모리 사용량 제어)
  - name: ARGOCD_REPO_SERVER_PARALLELISM_LIMIT
    value: "10"
```

`argocd_repo_pending_request_total` 메트릭이 지속적으로 높으면 repo-server 인스턴스를 늘리거나
parallelism-limit을 조정한다.

### 7.3 App Resync 주기 조정

소스 코드 (`cmd/argocd-application-controller/commands/argocd_application_controller.go:256-258`) 에서 확인된 설정:

```go
// app-resync: 기본값은 defaultAppResyncPeriod 초 (일반적으로 180초)
command.Flags().Int64Var(&appResyncPeriod, "app-resync",
    int64(env.ParseDurationFromEnv("ARGOCD_RECONCILIATION_TIMEOUT",
        defaultAppResyncPeriod*time.Second, 0, math.MaxInt64).Seconds()),
    "Time period in seconds for application resync.")

// app-resync-jitter: 부하 분산을 위한 지터
command.Flags().Int64Var(&appResyncJitter, "app-resync-jitter", ...)
```

```yaml
env:
  # 앱 조정 주기 (초) — 기본값 180초
  - name: ARGOCD_RECONCILIATION_TIMEOUT
    value: "300s"   # 5분으로 늘려 API 서버 부하 감소

  # 지터 (초) — 동시 조정 폭발 방지
  - name: ARGOCD_RECONCILIATION_JITTER
    value: "60s"
```

### 7.4 Redis 캐시 설정

Redis는 Argo CD에서 다음 데이터를 캐시한다:
- 레포지토리 매니페스트 생성 결과
- Kubernetes 클러스터 상태 캐시
- OCI 아티팩트 메타데이터

```yaml
# argocd-cmd-params-cm에서 Redis 설정
data:
  # Redis 주소 (기본값: argocd-redis:6379)
  redis.server: "argocd-redis:6379"

  # Redis 압축 방식 (none, gzip, zstd)
  redis.compression: "gzip"

  # 캐시 만료 시간 (기본값: 24시간)
  reposerver.default.cache.expiration: "24h"
```

Redis 메모리 설정:
```yaml
# Redis ConfigMap
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-redis-ha-configmap
data:
  redis.conf: |
    maxmemory 256mb
    maxmemory-policy allkeys-lru
    save ""
    appendonly no
```

### 7.5 클러스터 샤딩

application-controller를 여러 인스턴스로 실행하여 클러스터를 분산 처리한다.
소스 코드 (`controller/sharding/sharding.go:85-98`) 에서 확인된 알고리즘:

```go
func GetDistributionFunction(clusters clusterAccessor, apps appAccessor,
    shardingAlgorithm string, replicasCount int) DistributionFunction {
    switch shardingAlgorithm {
    case common.RoundRobinShardingAlgorithm:      // "round-robin"
        distributionFunction = RoundRobinDistributionFunction(clusters, replicasCount)
    case common.LegacyShardingAlgorithm:          // "legacy"
        distributionFunction = LegacyDistributionFunction(replicasCount)
    case common.ConsistentHashingWithBoundedLoadsAlgorithm:  // "consistent-hashing"
        distributionFunction = ConsistentHashingWithBoundedLoadsDistributionFunction(clusters, apps, replicasCount)
    }
}
```

소스 코드 (`common/common.go:144-156`) 에서 확인된 알고리즘 상수:
```go
LegacyShardingAlgorithm                    = "legacy"
RoundRobinShardingAlgorithm                = "round-robin"
ConsistentHashingWithBoundedLoadsAlgorithm = "consistent-hashing"
DefaultShardingAlgorithm                   = LegacyShardingAlgorithm
```

알고리즘 비교:

| 알고리즘 | 특성 | 권장 사용 |
|----------|------|----------|
| `legacy` (기본값) | 클러스터 UID 기반 해시, 균등하지 않을 수 있음 | 하위 호환 유지 시 |
| `round-robin` | 클러스터를 순서대로 분배, 균등한 분배 | 앱 수가 균등한 클러스터들 |
| `consistent-hashing` | 앱 수 기반 bounded load, 가장 균등 | 대규모 환경 |

샤딩 설정 방법:

```yaml
# application-controller StatefulSet
spec:
  replicas: 3  # 샤드 수
  template:
    spec:
      containers:
        - name: argocd-application-controller
          env:
            # 전체 레플리카 수
            - name: ARGOCD_CONTROLLER_REPLICAS
              value: "3"
            # 샤딩 알고리즘
            - name: ARGOCD_CONTROLLER_SHARDING_ALGORITHM
              value: "consistent-hashing"
            # 동적 클러스터 분배 활성화 (레플리카 변경 시 자동 재분배)
            - name: ARGOCD_ENABLE_DYNAMIC_CLUSTER_DISTRIBUTION
              value: "true"
```

소스 코드 (`controller/sharding/sharding.go:40-43`) 에서 확인된 하트비트 설정:
```go
HeartbeatDuration = env.ParseNumFromEnv(common.EnvControllerHeartbeatTime, 10, 10, 60)
HeartbeatTimeout  = 3 * HeartbeatDuration
```

샤드 분배는 `argocd-app-controller-lock` ConfigMap에 저장된다:
```bash
kubectl get configmap argocd-app-controller-lock -n argocd -o yaml
```

### 7.6 메모리 최적화

```yaml
# application-controller 리소스 설정
resources:
  requests:
    cpu: 250m
    memory: 512Mi
  limits:
    cpu: 2000m
    memory: 2Gi   # 관리하는 클러스터/앱 수에 따라 조정

# repo-server 리소스 설정
resources:
  requests:
    cpu: 250m
    memory: 256Mi
  limits:
    cpu: 1000m
    memory: 1Gi
```

---

## 8. 보안

### 8.1 TLS 설정

#### argocd-server TLS

```yaml
# argocd-server는 기본적으로 TLS를 활성화
# 인증서 설정 방법:

# 방법 1: Argo CD 자체 생성 인증서 사용 (기본값)
# argocd-tls-certs-cm에서 관리

# 방법 2: cert-manager를 이용한 자동 인증서
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: argocd-server-tls
  namespace: argocd
spec:
  secretName: argocd-server-tls
  dnsNames:
    - argocd.example.com
  issuerRef:
    name: letsencrypt-prod
    kind: ClusterIssuer

# 방법 3: 기존 인증서 Secret 마운트
# argocd-secret에서 tls.crt, tls.key 설정
```

#### repo-server TLS

```bash
# repo-server와의 통신에서 TLS 강제 검증 (argocd_application_controller.go:278-279)
# 환경 변수 설정
ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_PLAINTEXT=false   # TLS 사용 (기본값)
ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_STRICT_TLS=true   # 엄격한 TLS 검증
```

### 8.2 RBAC 최소 권한 원칙

**나쁜 예 (과도한 권한):**
```csv
p, role:developer, *, *, */*, allow
```

**좋은 예 (최소 권한):**
```csv
# 개발자: 자신의 프로젝트에서만, sync과 로그 조회만
p, role:developer, applications, get, dev-project/*, allow
p, role:developer, applications, sync, dev-project/*, allow
p, role:developer, logs, get, dev-project/*, allow
# exec 명시적 거부 (기본값이 deny라도 명시적으로 작성 권장)
p, role:developer, exec, create, */*, deny
```

**프로젝트 범위 격리:**
```yaml
# AppProject로 접근 가능한 클러스터/네임스페이스 제한
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: production
spec:
  # 이 프로젝트에서 배포 가능한 클러스터와 네임스페이스 제한
  destinations:
    - server: https://prod-cluster.example.com
      namespace: production
  # 소스 저장소 제한
  sourceRepos:
    - https://github.com/my-org/production-configs
  # 클러스터 리소스 생성/수정 제한
  clusterResourceWhitelist: []  # 클러스터 범위 리소스 생성 금지
  namespaceResourceBlacklist:
    - group: ""
      kind: ResourceQuota
```

### 8.3 시크릿 관리

Argo CD에서 시크릿을 안전하게 관리하는 방법:

#### argocd-vault-plugin 사용

```yaml
# Helm values.yaml에서 plugin 설정
repoServer:
  env:
    - name: VAULT_ADDR
      value: "https://vault.example.com"
  volumes:
    - name: argocd-vault-plugin
      configMap:
        name: argocd-vault-plugin-cm
  initContainers:
    - name: download-tools
      image: alpine:3.8
      command: [sh, -c]
      args:
        - wget -O /custom-tools/argocd-vault-plugin
            https://github.com/argoproj-labs/argocd-vault-plugin/releases/download/v1.x.x/argocd-vault-plugin_linux_amd64
```

#### External Secrets Operator (ESO) 사용

```yaml
# ExternalSecret으로 argocd-secret 자동 동기화
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: argocd-secret
  namespace: argocd
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: ClusterSecretStore
  target:
    name: argocd-secret
  data:
    - secretKey: server.secretkey
      remoteRef:
        key: argocd/server
        property: secret-key
    - secretKey: webhook.github.secret
      remoteRef:
        key: argocd/webhooks
        property: github-secret
```

#### argocd-cm에서 시크릿 참조

`$` 접두사로 `argocd-secret`의 값을 참조할 수 있다:

```yaml
# argocd-cm
data:
  dex.config: |
    connectors:
      - type: github
        config:
          # argocd-secret의 dex.github.clientID 값 참조
          clientID: $dex.github.clientID
          clientSecret: $dex.github.clientSecret
```

### 8.4 GPG 서명 검증

Argo CD는 Git 커밋의 GPG 서명을 검증하여 신뢰할 수 있는 커밋만 배포하도록 강제할 수 있다.

소스 코드에서 확인 (`cmd/argocd-repo-server/commands/argocd_repo_server.go:214`):
```go
go func() { errors.CheckError(reposerver.StartGPGWatcher(gnuPGSourcePath)) }()
```

```bash
# GPG 공개키 등록
argocd gpg add --from my-gpg-key.asc

# 등록된 GPG 키 목록 확인
argocd gpg list

# 특정 키 확인
argocd gpg get <key-id>

# GPG 키 삭제
argocd gpg rm <key-id>
```

AppProject에서 GPG 서명 강제:
```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: production
spec:
  # 서명된 커밋만 허용
  signatureKeys:
    - keyID: "D23B655E"   # 허용할 GPG 키 ID
```

### 8.5 Webhook 시크릿 검증

Git 저장소에서 Push 이벤트를 받을 때 Webhook 시크릿으로 요청을 검증한다.

소스 코드에서 확인된 시크릿 키들 (`util/settings/settings.go:447-453`):
```go
settingsWebhookGitHubSecretKey          = "webhook.github.secret"
settingsWebhookGitLabSecretKey          = "webhook.gitlab.secret"
settingsWebhookBitbucketUUIDKey         = "webhook.bitbucket.uuid"
settingsWebhookBitbucketServerSecretKey = "webhook.bitbucketserver.secret"
```

GitHub Webhook 설정:
1. GitHub 저장소 → Settings → Webhooks → Add webhook
2. Payload URL: `https://argocd.example.com/api/webhook`
3. Content type: `application/json`
4. Secret: `argocd-secret`의 `webhook.github.secret` 와 동일한 값
5. 이벤트: Push events (최소)

```bash
# argocd-secret에 webhook 시크릿 설정
kubectl patch secret argocd-secret -n argocd \
  --type='json' \
  -p='[{"op":"add","path":"/data/webhook.github.secret","value":"'$(echo -n "my-webhook-secret" | base64)'"}]'
```

### 8.6 네트워크 정책

```yaml
# argocd-server에 대한 외부 접근 제한
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: argocd-server-network-policy
  namespace: argocd
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: argocd-server
  policyTypes:
    - Ingress
  ingress:
    # UI/API 접근 (443/80)
    - ports:
        - port: 8080
        - port: 8443
    # 메트릭 수집 (Prometheus)
    - from:
        - namespaceSelector:
            matchLabels:
              name: monitoring
      ports:
        - port: 8083

---
# application-controller는 내부 통신만 허용
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: argocd-application-controller-network-policy
  namespace: argocd
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: argocd-application-controller
  policyTypes:
    - Ingress
  ingress:
    # 메트릭만 허용
    - from:
        - namespaceSelector:
            matchLabels:
              name: monitoring
      ports:
        - port: 8082
```

### 8.7 보안 감사 체크리스트

| 항목 | 확인 사항 |
|------|----------|
| 인증 | SSO 연동 완료, 익명 접근 비활성화 (`users.anonymous.enabled: "false"`) |
| JWT 키 | `server.secretkey`가 충분히 강한 무작위 값인지 확인 |
| RBAC | `role:admin` 사용자 최소화, 프로젝트별 권한 격리 |
| TLS | HTTPS 강제, 자체 서명 인증서 미사용 |
| Webhook | 모든 Webhook에 시크릿 설정 |
| exec | `exec.enabled: "false"` (불필요 시) |
| in-cluster | `cluster.inClusterEnabled` 필요 여부 검토 |
| GPG | 중요 환경에서 커밋 서명 검증 활성화 |
| 네트워크 정책 | 컴포넌트 간 불필요한 통신 차단 |
| 감사 로그 | Kubernetes 감사 로그에서 Argo CD 액션 추적 |
| 시크릿 관리 | Git에 시크릿 직접 커밋 금지, ESO 또는 Vault 사용 |
| 정기 점검 | RBAC 정책 정기 검토, 사용하지 않는 계정 비활성화 |

---

## 참고 자료

- Argo CD 공식 문서: https://argo-cd.readthedocs.io/
- RBAC 설정 참조: `assets/builtin-policy.csv`
- 설정 키 정의: `util/settings/settings.go`
- RBAC 상수 정의: `util/rbac/rbac.go`
- 샤딩 알고리즘: `controller/sharding/sharding.go`
- 메트릭 정의: `controller/metrics/metrics.go`, `reposerver/metrics/metrics.go`
- JWT/세션 관리: `util/session/sessionmanager.go`
- OpenTelemetry 추적: `util/trace/trace.go`
