# Grafana 운영 가이드

## 개요

이 문서는 Grafana의 배포, 설정, 데이터베이스, 인증, 프로비저닝, 모니터링, 트러블슈팅, 고가용성, 보안 운영에 대한 포괄적인 가이드를 제공한다. 모든 설정 참조는 Grafana 소스 코드의 `conf/defaults.ini`와 `pkg/setting/setting.go`에서 직접 검증한 것이다.

---

## 1. 배포 옵션

### 1.1 바이너리 설치

Grafana는 단일 Go 바이너리로 컴파일되며, 별도의 런타임 의존성이 없다.

```bash
# 빌드
make build-backend

# 실행
./bin/grafana server \
    --homepath=/usr/share/grafana \
    --config=/etc/grafana/grafana.ini \
    --pidfile=/var/run/grafana/grafana-server.pid
```

빌드 시 ldflags로 버전 정보가 주입된다:

```makefile
GO_LDFLAGS = -X main.version=$(BUILD_VERSION) \
    -X main.commit=$(BUILD_COMMIT) \
    -X main.buildBranch=$(BUILD_BRANCH) \
    -X main.buildstamp=$(BUILD_STAMP)
```

### 1.2 Docker 설치

```bash
# 기본 실행
docker run -d \
    --name grafana \
    -p 3000:3000 \
    grafana/grafana:latest

# 영구 볼륨과 설정 마운트
docker run -d \
    --name grafana \
    -p 3000:3000 \
    -v grafana-data:/var/lib/grafana \
    -v /path/to/grafana.ini:/etc/grafana/grafana.ini \
    -v /path/to/provisioning:/etc/grafana/provisioning \
    -e GF_SECURITY_ADMIN_PASSWORD=mysecret \
    grafana/grafana:latest
```

### 1.3 Helm Chart (Kubernetes)

```bash
# Helm 저장소 추가
helm repo add grafana https://grafana.github.io/helm-charts
helm repo update

# 기본 설치
helm install grafana grafana/grafana \
    --namespace monitoring \
    --create-namespace

# values.yaml 커스터마이징
helm install grafana grafana/grafana \
    --namespace monitoring \
    -f values.yaml
```

`values.yaml` 예시:

```yaml
replicas: 2

persistence:
  enabled: true
  size: 10Gi

datasources:
  datasources.yaml:
    apiVersion: 1
    datasources:
      - name: Prometheus
        type: prometheus
        url: http://prometheus-server:9090
        isDefault: true

grafana.ini:
  server:
    domain: grafana.example.com
    root_url: https://grafana.example.com
  database:
    type: postgres
    host: postgres:5432
    name: grafana
    user: grafana
    password: $__vault{database/grafana}
```

### 1.4 APT/YUM 패키지

```bash
# APT (Debian/Ubuntu)
sudo apt-get install -y apt-transport-https software-properties-common
sudo apt-get install grafana

# YUM (RHEL/CentOS)
sudo yum install grafana

# systemd 서비스 관리
sudo systemctl start grafana-server
sudo systemctl enable grafana-server
sudo systemctl status grafana-server
```

---

## 2. 설정

### 2.1 설정 파일 위치

| 파일 | 경로 | 용도 |
|------|------|------|
| defaults.ini | `conf/defaults.ini` | 기본 설정값 (수정 불가) |
| grafana.ini | `/etc/grafana/grafana.ini` | 사용자 정의 설정 |
| custom.ini | `conf/custom.ini` | 커스텀 오버라이드 |

### 2.2 설정 로딩 우선순위

설정은 다음 순서로 로딩되며, 후순위가 전순위를 오버라이드한다:

```
1. conf/defaults.ini              (최저 우선순위)
2. conf/grafana.ini 또는 --config
3. conf/custom.ini
4. GF_{SECTION}_{KEY} 환경변수
5. CLI 플래그                      (최고 우선순위)
```

소스 코드에서 확인한 로딩 흐름 (`pkg/setting/setting.go`):

```go
func (cfg *Cfg) loadConfiguration(args CommandLineArgs) (*ini.File, error) {
    // 1. defaults.ini 로드
    defaultConfigFile := path.Join(cfg.HomePath, "conf/defaults.ini")
    parsedFile, err := ini.Load(defaultConfigFile)

    // 2. 명령행 기본 속성 적용
    commandLineProps := cfg.getCommandLineProperties(args.Args)
    cfg.applyCommandLineDefaultProperties(commandLineProps, parsedFile)

    // 3. 사용자 설정 파일 로드
    err = cfg.loadSpecifiedConfigFile(args.Config, parsedFile)

    // 4. 환경 변수 오버라이드
    err = cfg.applyEnvVariableOverrides(parsedFile)

    // 5. 명령행 오버라이드
    cfg.applyCommandLineProperties(commandLineProps, parsedFile)

    // 6. 환경 변수 값 확장
    err = expandConfig(parsedFile)

    return parsedFile, err
}
```

### 2.3 환경변수 오버라이드

모든 설정은 `GF_{SECTION}_{KEY}` 패턴의 환경변수로 오버라이드할 수 있다:

```bash
# [server] 섹션
GF_SERVER_HTTP_PORT=8080
GF_SERVER_DOMAIN=grafana.example.com
GF_SERVER_ROOT_URL=https://grafana.example.com
GF_SERVER_PROTOCOL=https
GF_SERVER_CERT_FILE=/path/to/cert.pem
GF_SERVER_CERT_KEY=/path/to/key.pem

# [database] 섹션
GF_DATABASE_TYPE=postgres
GF_DATABASE_HOST=db.example.com:5432
GF_DATABASE_NAME=grafana
GF_DATABASE_USER=grafana
GF_DATABASE_PASSWORD=secret

# [security] 섹션
GF_SECURITY_ADMIN_USER=admin
GF_SECURITY_ADMIN_PASSWORD=strongpassword
GF_SECURITY_SECRET_KEY=myrandomsecretkey

# [auth.github] 섹션 (점은 언더스코어로)
GF_AUTH_GITHUB_ENABLED=true
GF_AUTH_GITHUB_CLIENT_ID=myclientid
GF_AUTH_GITHUB_CLIENT_SECRET=myclientsecret

# [log] 섹션
GF_LOG_LEVEL=debug
GF_LOG_MODE=console
```

### 2.4 주요 설정 섹션

#### [server] - HTTP 서버

```ini
[server]
# 프로토콜: http, https, h2, socket, socket_h2
protocol = http

# 바인딩 주소 (빈 값 = 모든 인터페이스)
http_addr =

# HTTP 포트
http_port = 3000

# 퍼블릭 도메인
domain = localhost

# 전체 퍼블릭 URL
root_url = %(protocol)s://%(domain)s:%(http_port)s/

# 서브패스에서 서빙
serve_from_sub_path = false

# Gzip 압축
enable_gzip = false

# HTTPS 인증서
cert_file =
cert_key =
cert_pass =

# 인증서 변경 감시 간격
certs_watch_interval =

# 읽기 타임아웃 (분, 0 = 무제한)
read_timeout = 0

# 정적 파일 경로
static_root_path = public
```

#### [database] - 데이터베이스

```ini
[database]
# DB 종류: sqlite3, postgres, mysql
type = sqlite3

# DB 호스트
host = 127.0.0.1:3306

# DB 이름
name = grafana

# DB 사용자/비밀번호
user = root
password =

# 또는 URL 형식
# url = postgres://user:secret@host:port/database

# 고가용성 모드
high_availability = true

# 커넥션 풀 설정
max_idle_conn = 2
max_open_conn =
conn_max_lifetime = 14400

# SQL 쿼리 로깅
log_queries =

# SSL 모드 (postgres: disable/require/verify-full)
ssl_mode = disable

# SQLite 경로 (data_path 상대)
path = grafana.db

# SQLite WAL 모드
wal = false

# 마이그레이션 잠금
migration_locking = true
locking_attempt_timeout_sec = 0

# 마이그레이션 건너뛰기
skip_migrations = false
```

#### [security] - 보안

```ini
[security]
# 관리자 기본 계정
admin_user = admin
admin_password = admin
admin_email = admin@localhost

# 서명용 비밀 키
secret_key = SW2YcwTIb9zpOOhoPsMm

# 봉투 암호화 제공자
encryption_provider = secretKey.v1

# 브루트포스 보호
disable_brute_force_login_protection = false
brute_force_login_protection_max_attempts = 5

# 쿠키 보안
cookie_secure = false
cookie_samesite = lax

# 임베딩 허용
allow_embedding = false

# HSTS
strict_transport_security = false
strict_transport_security_max_age_seconds = 86400

# X-Content-Type-Options
x_content_type_options = true

# X-XSS-Protection
x_xss_protection = true

# Content Security Policy
content_security_policy = false
content_security_policy_template = """script-src 'self' 'unsafe-eval' ..."""
```

#### [paths] - 경로

```ini
[paths]
# 데이터 디렉토리 (SQLite DB, 세션, 플러그인)
data = data

# 임시 데이터 TTL
temp_data_lifetime = 24h

# 로그 디렉토리
logs = data/log

# 플러그인 디렉토리
plugins = data/plugins

# 프로비저닝 설정 디렉토리
provisioning = conf/provisioning
```

#### [log] - 로깅

```ini
[log]
# 로그 모드: console, file, syslog (공백 구분)
mode = console file

# 로그 레벨: debug, info, warn, error
level = info

# 특정 로거 필터
# 예: filters = sqlstore:debug alerting:warn
filters =

# 사용자 표시용 기본 에러 메시지
user_facing_default_error = "please inspect Grafana server log for details"

[log.console]
level =
format = console  # text, console, json
```

#### [metrics] - 메트릭

```ini
[metrics]
enabled = true
interval_seconds = 10

# 전체 통계 비활성화
disable_total_stats = false

# 통계 수집 간격
total_stats_collector_interval_seconds = 1800
```

#### [plugins] - 플러그인

```ini
[plugins]
# 알파 플러그인 활성화
enable_alpha = false

# 서명 없는 플러그인 허용 (쉼표 구분 ID)
allow_loading_unsigned_plugins =

# 플러그인 관리 UI 활성화
plugin_admin_enabled = true
plugin_catalog_url = https://grafana.com/grafana/plugins/
```

---

## 3. 데이터베이스

### 3.1 지원 데이터베이스

| DB | 용도 | 프로덕션 권장 | 기본값 |
|----|------|-------------|--------|
| SQLite3 | 단일 인스턴스, 개발 | 아니오 | 예 |
| PostgreSQL | 프로덕션 | 예 | 아니오 |
| MySQL/MariaDB | 프로덕션 대안 | 예 | 아니오 |

### 3.2 SQLite (기본)

```ini
[database]
type = sqlite3
path = grafana.db
# WAL 모드 (동시 읽기 성능 향상)
wal = false
# 캐시 모드: private (기본), shared
cache_mode = private
# 잠금 실패 시 재시도 횟수
query_retries = 0
transaction_retries = 5
```

SQLite는 `{data_path}/grafana.db`에 저장되며, 단일 인스턴스에 적합하다. 고가용성이 필요한 경우 PostgreSQL이나 MySQL로 마이그레이션해야 한다.

### 3.3 PostgreSQL (프로덕션 권장)

```ini
[database]
type = postgres
host = db.example.com:5432
name = grafana
user = grafana
password = secretpassword
ssl_mode = require

# 또는 URL 형식
url = postgres://grafana:secretpassword@db.example.com:5432/grafana?sslmode=require
```

### 3.4 MySQL

```ini
[database]
type = mysql
host = db.example.com:3306
name = grafana
user = grafana
password = secretpassword
ssl_mode = true
```

### 3.5 커넥션 풀 설정

```ini
[database]
# 최대 유휴 커넥션
max_idle_conn = 2

# 최대 열린 커넥션 (0 = 무제한)
max_open_conn =

# 커넥션 최대 수명 (초, 기본 4시간)
conn_max_lifetime = 14400
```

프로덕션 환경에서 권장하는 커넥션 풀 설정:

```ini
max_idle_conn = 25
max_open_conn = 100
conn_max_lifetime = 14400
```

### 3.6 마이그레이션 시스템

Grafana는 서버 시작 시 자동으로 데이터베이스 마이그레이션을 실행한다. `pkg/services/sqlstore/migrations/` 디렉토리에 도메인별 마이그레이션 파일이 있다:

```
migrations/
├── accesscontrol/           # RBAC 테이블
├── alert_mig.go             # 알림 테이블
├── annotation_mig.go        # 어노테이션 테이블
├── apikey_mig.go             # API 키 테이블
├── dashboard_mig.go          # 대시보드 테이블
├── dashboard_acl.go          # 대시보드 ACL
├── dashboard_version_mig.go  # 대시보드 버전
├── datasource_mig.go         # 데이터소스 테이블
├── folder_mig.go             # 폴더 테이블
└── common.go                # 공통 유틸
```

마이그레이션 실행 흐름 (소스: `pkg/services/sqlstore/sqlstore.go`):

```go
func (ss *SQLStore) Migrate(isDatabaseLockingEnabled bool) error {
    if ss.dbCfg.SkipMigrations || ss.migrations == nil {
        return nil
    }

    migrator := migrator.NewMigrator(ss.engine, ss.cfg)
    ss.migrations.AddMigration(migrator)

    return migrator.RunMigrations(ctx, isDatabaseLockingEnabled,
        ss.dbCfg.MigrationLockAttemptTimeout)
}
```

마이그레이션 로그는 `migration_log` 테이블에 기록된다:

```go
type MigrationLog struct {
    Id          int64
    MigrationID string `xorm:"migration_id"`
    SQL         string `xorm:"sql"`
    Success     bool
    Error       string
    Timestamp   time.Time
}
```

마이그레이션 잠금 설정:

```ini
[database]
# 다중 인스턴스 동시 마이그레이션 방지
migration_locking = true
# 잠금 대기 타임아웃 (초)
locking_attempt_timeout_sec = 0
# 마이그레이션 건너뛰기 (주의: 스키마 불일치 위험)
skip_migrations = false
```

---

## 4. 인증 설정

### 4.1 지원 인증 방식

| 방식 | 설정 섹션 | 용도 |
|------|----------|------|
| Basic Auth | `[auth.basic]` | 기본 사용자/비밀번호 인증 |
| OAuth - GitHub | `[auth.github]` | GitHub 소셜 로그인 |
| OAuth - GitLab | `[auth.gitlab]` | GitLab 소셜 로그인 |
| OAuth - Google | `[auth.google]` | Google 소셜 로그인 |
| OAuth - Azure AD | `[auth.azuread]` | Microsoft Azure AD |
| OAuth - Okta | `[auth.okta]` | Okta SSO |
| OAuth - Generic | `[auth.generic_oauth]` | 범용 OAuth2 |
| LDAP | `[auth.ldap]` | LDAP/Active Directory |
| JWT | `[auth.jwt]` | JWT 토큰 인증 |
| Auth Proxy | `[auth.proxy]` | 리버스 프록시 인증 |
| Anonymous | `[auth.anonymous]` | 익명 접근 허용 |
| Passwordless | `[auth.passwordless]` | 비밀번호 없는 인증 |

### 4.2 Basic Auth

```ini
[auth.basic]
enabled = true
```

### 4.3 OAuth - GitHub

```ini
[auth.github]
name = GitHub
icon = github
enabled = true
allow_sign_up = true
auto_login = false
client_id = your_github_client_id
client_secret = your_github_client_secret
scopes = user:email,read:org
auth_url = https://github.com/login/oauth/authorize
token_url = https://github.com/login/oauth/access_token
api_url = https://api.github.com/user
allowed_organizations = myorg
team_ids = 123,456
role_attribute_path = role
allow_assign_grafana_admin = false
```

### 4.4 OAuth - Google

```ini
[auth.google]
name = Google
icon = google
enabled = true
allow_sign_up = true
client_id = your_google_client_id
client_secret = your_google_client_secret
scopes = openid email profile
auth_url = https://accounts.google.com/o/oauth2/v2/auth
token_url = https://oauth2.googleapis.com/token
api_url = https://openidconnect.googleapis.com/v1/userinfo
allowed_domains = example.com
hosted_domain = example.com
use_pkce = true
use_refresh_token = true
```

### 4.5 OAuth - Azure AD

```ini
[auth.azuread]
name = Microsoft
icon = microsoft
enabled = true
allow_sign_up = true
client_id = your_azure_client_id
client_secret = your_azure_client_secret
scopes = openid email profile
auth_url = https://login.microsoftonline.com/{tenant-id}/oauth2/v2.0/authorize
token_url = https://login.microsoftonline.com/{tenant-id}/oauth2/v2.0/token
allowed_organizations = your-tenant-id
use_pkce = true
use_refresh_token = true
```

### 4.6 LDAP

```ini
[auth.ldap]
enabled = true
config_file = /etc/grafana/ldap.toml
allow_sign_up = true
```

LDAP 설정 파일 (`ldap.toml`):

```toml
[[servers]]
host = "ldap.example.com"
port = 636
use_ssl = true
start_tls = false
ssl_skip_verify = false

bind_dn = "cn=admin,dc=example,dc=com"
bind_password = "secretpassword"

search_filter = "(uid=%s)"
search_base_dns = ["dc=example,dc=com"]

[servers.attributes]
name = "givenName"
surname = "sn"
username = "uid"
member_of = "memberOf"
email = "mail"

[[servers.group_mappings]]
group_dn = "cn=admins,ou=groups,dc=example,dc=com"
org_role = "Admin"
grafana_admin = true

[[servers.group_mappings]]
group_dn = "cn=editors,ou=groups,dc=example,dc=com"
org_role = "Editor"

[[servers.group_mappings]]
group_dn = "*"
org_role = "Viewer"
```

### 4.7 JWT

```ini
[auth.jwt]
enabled = true
enable_login_token = false
header_name = X-JWT-Assertion
email_claim = email
username_claim = sub
jwk_set_url = https://your-idp.example.com/.well-known/jwks.json
```

### 4.8 Auth Proxy

리버스 프록시(Nginx, Apache 등)에서 인증을 처리하고 헤더로 사용자 정보를 전달:

```ini
[auth.proxy]
enabled = true
header_name = X-WEBAUTH-USER
header_property = username
auto_sign_up = true
headers = Email:X-User-Email Name:X-User-Name
```

### 4.9 공통 인증 설정

```ini
[auth]
# 로그인 쿠키 이름
login_cookie_name = grafana_session

# 비활성 사용자 최대 수명 (기본 7일)
login_maximum_inactive_lifetime_duration =

# 로그인 최대 수명 (기본 30일)
login_maximum_lifetime_duration =

# 토큰 로테이션 간격 (분)
token_rotation_interval_minutes = 10

# 로그인 폼 비활성화 (OAuth만 사용 시)
disable_login_form = false

# OAuth 자동 로그인
oauth_auto_login = false

# API 키 최대 수명 (초, -1 = 무제한)
api_key_max_seconds_to_live = -1
```

---

## 5. 프로비저닝

### 5.1 프로비저닝 개요

프로비저닝은 YAML 파일을 통해 대시보드, 데이터소스, 알림 규칙 등을 선언적으로 배포하는 기능이다. `conf/provisioning/` 디렉토리 아래에 카테고리별로 설정한다.

```
conf/provisioning/
├── access-control/     # 접근 제어 정책
├── alerting/           # 알림 규칙, 알림 채널
├── dashboards/         # 대시보드 프로비저닝
├── datasources/        # 데이터소스 프로비저닝
├── plugins/            # 플러그인 설정
└── sample/             # 샘플 파일
```

### 5.2 데이터소스 프로비저닝

`conf/provisioning/datasources/` 아래에 YAML 파일을 배치한다:

```yaml
# conf/provisioning/datasources/prometheus.yaml
apiVersion: 1

deleteDatasources:
  - name: OldPrometheus
    orgId: 1

datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    orgId: 1
    uid: prometheus-main
    url: http://prometheus-server:9090
    isDefault: true
    jsonData:
      httpMethod: POST
      timeInterval: 15s
    editable: false

  - name: Loki
    type: loki
    access: proxy
    orgId: 1
    uid: loki-main
    url: http://loki-gateway:3100
    jsonData:
      maxLines: 1000
    editable: false
```

### 5.3 대시보드 프로비저닝

```yaml
# conf/provisioning/dashboards/default.yaml
apiVersion: 1

providers:
  - name: Default
    orgId: 1
    folder: Provisioned
    folderUid: provisioned-dashboards
    type: file
    disableDeletion: true
    updateIntervalSeconds: 30
    allowUiUpdates: false
    options:
      path: /var/lib/grafana/dashboards
      foldersFromFilesStructure: true
```

### 5.4 알림 규칙 프로비저닝

```yaml
# conf/provisioning/alerting/rules.yaml
apiVersion: 1

groups:
  - orgId: 1
    name: HighCPU
    folder: Alerts
    interval: 1m
    rules:
      - uid: cpu-high
        title: CPU Usage High
        condition: C
        data:
          - refId: A
            datasourceUid: prometheus-main
            model:
              expr: node_cpu_seconds_total
```

### 5.5 프로비저닝 설정

```ini
[paths]
# 프로비저닝 설정 디렉토리
provisioning = conf/provisioning
```

---

## 6. 모니터링

### 6.1 헬스체크 엔드포인트

| 엔드포인트 | 메서드 | 설명 | 인증 |
|-----------|--------|------|------|
| `/api/health` | GET | API 헬스체크 | 불필요 |
| `/healthz` | GET | K8s liveness probe | 불필요 |
| `/readyz` | GET | K8s readiness probe | 불필요 |
| `/metrics` | GET | Prometheus 메트릭 | 선택 (Basic Auth) |

소스 코드에서 확인한 헬스체크 구현 (`pkg/server/health.go`):

```go
// LivezHandler - 프로세스 생존 확인 (항상 200 OK)
func LivezHandler() http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("OK"))
    }
}

// ReadyzHandler - 준비 상태 확인 (200 또는 503)
func ReadyzHandler(h *HealthNotifier) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if h != nil && h.IsReady() {
            w.WriteHeader(http.StatusOK)
        } else {
            w.WriteHeader(http.StatusServiceUnavailable)
        }
    }
}
```

### 6.2 Prometheus 메트릭

`/metrics` 엔드포인트에서 Prometheus 형식의 메트릭을 노출한다.

핵심 메트릭:

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `grafana_http_request_duration_seconds` | Histogram | HTTP 요청 처리 시간 |
| `grafana_http_request_in_flight` | Gauge | 진행 중인 HTTP 요청 수 |
| `grafana_html_handler_requests_duration_seconds` | Histogram | HTML 핸들러 처리 시간 |
| `grafana_ds_config_handler_requests_duration_seconds` | Histogram | 데이터소스 설정 핸들러 시간 |
| `grafana_stat_totals_dashboard` | Gauge | 전체 대시보드 수 |
| `grafana_stat_totals_datasource` | Gauge | 전체 데이터소스 수 |
| `grafana_stat_totals_users` | Gauge | 전체 사용자 수 |
| `grafana_stat_totals_orgs` | Gauge | 전체 조직 수 |
| `grafana_api_response_status_total` | Counter | API 응답 상태별 카운트 |

메트릭 설정:

```ini
[metrics]
# 메트릭 수집 활성화
enabled = true

# 수집 간격 (초)
interval_seconds = 10

# 전체 통계 비활성화
disable_total_stats = false

# 통계 수집기 간격 (초)
total_stats_collector_interval_seconds = 1800

[metrics.graphite]
# Graphite로 메트릭 전송
address =
prefix = prod.grafana.%(instance_name)s

[metrics.environment_info]
# 메트릭에 포함할 환경 정보 레이블
# 예: exampleLabel1 = exampleValue1
```

메트릭 엔드포인트 Basic Auth 설정:

```ini
[metrics]
basic_auth_username = metrics_user
basic_auth_password = metrics_password
```

소스 코드에서 확인한 메트릭 엔드포인트 구현:

```go
// pkg/api/http_server.go
func (hs *HTTPServer) metricsEndpoint(ctx *web.Context) {
    if !hs.Cfg.MetricsEndpointEnabled {
        return
    }

    if ctx.Req.Method != http.MethodGet || ctx.Req.URL.Path != "/metrics" {
        return
    }

    // Basic Auth 검증
    if hs.metricsEndpointBasicAuthEnabled() &&
        !BasicAuthenticatedRequest(ctx.Req, ...) {
        ctx.Resp.Header().Set("WWW-Authenticate", `Basic realm="Grafana"`)
        ctx.Resp.WriteHeader(http.StatusUnauthorized)
        return
    }

    promhttp.HandlerFor(hs.promGatherer, promhttp.HandlerOpts{
        EnableOpenMetrics: true,
    }).ServeHTTP(ctx.Resp, ctx.Req)
}
```

### 6.3 Grafana 자체 모니터링 대시보드

Grafana mixin(`grafana-mixin/`)을 사용하여 Grafana 자체를 모니터링하는 대시보드를 생성할 수 있다.

---

## 7. 트러블슈팅

### 7.1 로그 설정

```ini
[log]
# 로그 출력 대상 (공백 구분)
mode = console file

# 전역 로그 레벨
level = info

# 특정 로거 필터 (패키지별 레벨 설정)
filters = sqlstore:debug alerting:warn rendering:debug
```

디버그 모드 활성화:

```bash
# 환경 변수로 디버그 로그 활성화
GF_LOG_LEVEL=debug ./bin/grafana server
```

### 7.2 프로파일링

Grafana는 Go의 `pprof`를 통한 런타임 프로파일링을 지원한다:

```ini
[diagnostics.profiling]
# pprof 프로파일링 활성화
enabled = false
# pprof 서버 주소
addr = localhost
# pprof 서버 포트
port = 6060
```

프로파일링 활성화 후 접근:

```bash
# CPU 프로파일
curl http://localhost:6060/debug/pprof/profile?seconds=30 > cpu.prof

# 메모리 프로파일
curl http://localhost:6060/debug/pprof/heap > heap.prof

# 고루틴 프로파일
curl http://localhost:6060/debug/pprof/goroutine?debug=1

# Go 도구로 분석
go tool pprof cpu.prof
```

### 7.3 분산 트레이싱

Grafana는 OpenTelemetry를 통한 분산 트레이싱을 지원한다:

```ini
# Jaeger (레거시)
[tracing.jaeger]
address = localhost:6831
sampler_type = const
sampler_param = 1

# OpenTelemetry (권장)
[tracing.opentelemetry]
custom_attributes = env:production,service:grafana
sampler_type = probabilistic
sampler_param = 0.1

# OTLP gRPC 내보내기
[tracing.opentelemetry.otlp]
address = localhost:4317
propagation = w3c

# OTLP Jaeger 내보내기
[tracing.opentelemetry.jaeger]
address = http://localhost:14268/api/traces
propagation = w3c
```

### 7.4 일반적인 문제와 해결

| 문제 | 원인 | 해결 |
|------|------|------|
| 포트 3000 사용 중 | 다른 프로세스 점유 | `GF_SERVER_HTTP_PORT=3001` 또는 프로세스 종료 |
| DB 마이그레이션 실패 | DB 권한 부족 | DB 사용자에 DDL 권한 부여 |
| 플러그인 로드 실패 | 서명 없는 플러그인 | `allow_loading_unsigned_plugins` 설정 |
| 메모리 부족 | 대규모 대시보드/쿼리 | `rendering` 설정 제한, 쿼리 타임아웃 |
| LDAP 연결 실패 | 인증서/방화벽 | `ldap.toml` SSL 설정 확인 |
| OAuth 콜백 에러 | root_url 불일치 | `root_url` 설정 확인 |
| 세션 유지 실패 | 멀티 인스턴스 | DB 세션 또는 Redis 원격 캐시 사용 |

### 7.5 지원 번들 생성

```bash
# API를 통한 지원 번들 생성
curl -X POST http://admin:admin@localhost:3000/api/support-bundles

# 번들 다운로드
curl http://admin:admin@localhost:3000/api/support-bundles/{uid}
```

---

## 8. 고가용성 (HA)

### 8.1 아키텍처

```
                    ┌──────────────┐
                    │ Load Balancer│
                    └──────┬───────┘
                    ┌──────┴───────┐
              ┌─────┤              ├─────┐
              │     │              │     │
        ┌─────▼─┐ ┌─▼──────┐ ┌────▼──┐  │
        │Grafana│ │Grafana │ │Grafana│  │
        │  #1   │ │  #2    │ │  #3   │  │
        └───┬───┘ └───┬────┘ └───┬───┘  │
            │         │          │       │
            └─────────┼──────────┘       │
                      │                  │
                ┌─────▼──────┐    ┌──────▼─────┐
                │ PostgreSQL │    │ Redis/     │
                │ (공유 DB)  │    │ Memcached  │
                └────────────┘    │ (세션 캐시)│
                                  └────────────┘
```

### 8.2 필수 구성

1. **공유 데이터베이스**: 모든 인스턴스가 동일한 PostgreSQL/MySQL에 연결
2. **원격 캐시**: 세션 공유를 위한 Redis 또는 Memcached
3. **로드 밸런서**: 스티키 세션 불필요 (세션이 DB/Redis에 저장)

### 8.3 데이터베이스 HA 설정

```ini
[database]
type = postgres
host = pgbouncer.example.com:5432
name = grafana
user = grafana
password = secret
high_availability = true
```

`high_availability = true`는 서버 잠금, 알림 평가 등에서 다중 인스턴스 동작을 활성화한다.

### 8.4 원격 캐시 설정

```ini
[remote_cache]
# Redis
type = redis
connstr = network=tcp,addr=redis:6379,pool_size=100,db=0,ssl=false

# 또는 Memcached
type = memcached
connstr = memcached:11211

# 캐시 키 접두사 (환경별 분리)
prefix = grafana_prod_

# 캐시 값 암호화
encryption = true
```

### 8.5 서버 잠금 (ServerLock)

`pkg/infra/serverlock/` 패키지가 다중 인스턴스 환경에서 작업 중복 실행을 방지한다. 데이터베이스를 사용하여 분산 잠금을 구현한다.

```ini
[auth]
# OAuth 토큰 갱신 시 서버 잠금 최소 대기 시간 (ms)
oauth_refresh_token_server_lock_min_wait_ms = 1000
```

### 8.6 SSO 설정 동기화

```ini
[sso_settings]
# SSO 설정 변경 폴링 간격 (HA 환경)
reload_interval = 1m
# API/UI로 설정 가능한 OAuth 제공자
configurable_providers = github gitlab google generic_oauth azuread okta
```

### 8.7 알림 HA

```ini
[unified_alerting]
enabled = true
# 알림 서비스 초기화 타임아웃
initialization_timeout = 30s
# 관리자 설정 폴링 간격
admin_config_poll_interval = 60s
```

---

## 9. 보안

### 9.1 Content Security Policy (CSP)

```ini
[security]
# CSP 활성화
content_security_policy = true

# CSP 정책 템플릿
content_security_policy_template = """script-src 'self' 'unsafe-eval' 'unsafe-inline' \
  'strict-dynamic' $NONCE;object-src 'none';font-src 'self';style-src 'self' \
  'unsafe-inline' blob:;img-src * data:;base-uri 'self';connect-src 'self' \
  grafana.com ws://$ROOT_PATH wss://$ROOT_PATH;manifest-src 'self'; \
  media-src 'none';form-action 'self';"""

# CSP 보고 전용 모드
content_security_policy_report_only = false
```

### 9.2 CSRF 보호

```ini
[security]
# 항상 CSRF 체크 실행 (로그인 쿠키 없어도)
csrf_always_check = false
```

소스 코드에서 CSRF 미들웨어 적용 확인:

```go
// pkg/api/http_server.go (addMiddlewaresAndStaticRoutes)
m.UseMiddleware(hs.Csrf.Middleware())
```

### 9.3 Security Headers

소스 코드 (`pkg/api/http_server.go`)에서 확인한 보안 헤더 설정:

```ini
[security]
# X-Content-Type-Options: nosniff
x_content_type_options = true

# X-XSS-Protection: 1; mode=block
x_xss_protection = true

# HSTS (HTTP Strict Transport Security)
strict_transport_security = true
strict_transport_security_max_age_seconds = 86400
strict_transport_security_preload = false
strict_transport_security_subdomains = false

# 임베딩 방지 (X-Frame-Options)
allow_embedding = false
```

### 9.4 TLS 설정

```ini
[server]
protocol = https
cert_file = /path/to/cert.pem
cert_key = /path/to/key.pem
cert_pass = optional_password

# 최소 TLS 버전
min_tls_version = TLS1.2

# 인증서 자동 갱신 감시 간격
certs_watch_interval = 10s
```

### 9.5 비밀 키 관리 (Envelope Encryption)

Grafana는 Envelope Encryption을 사용하여 민감한 데이터를 암호화한다:

```ini
[security]
# 마스터 비밀 키 (서명 및 암호화)
secret_key = your_random_secret_key_here

# 봉투 암호화 제공자
encryption_provider = secretKey.v1

# Enterprise: 외부 KMS 제공자
# available_encryption_providers = awskms.v1 azurekv.v1

[security.encryption]
# 복호화 키 캐시 TTL
data_keys_cache_ttl = 15m

# 캐시 정리 간격
data_keys_cache_cleanup_interval = 1m
```

### 9.6 쿠키 보안

```ini
[security]
# HTTPS 전용 쿠키
cookie_secure = true

# SameSite 속성: lax, strict, none, disabled
cookie_samesite = lax
```

### 9.7 브루트포스 보호

```ini
[security]
# 브루트포스 로그인 방지
disable_brute_force_login_protection = false

# 최대 실패 시도 횟수
brute_force_login_protection_max_attempts = 5

# 사용자명 기반 보호
disable_username_login_protection = false

# IP 주소 기반 보호
disable_ip_address_login_protection = true
```

### 9.8 보안 점검 체크리스트

| 항목 | 권장 설정 | 확인 방법 |
|------|----------|----------|
| 관리자 비밀번호 변경 | 기본값(`admin`) 변경 필수 | UI 또는 `GF_SECURITY_ADMIN_PASSWORD` |
| HTTPS 사용 | `protocol = https` | 인증서 파일 경로 확인 |
| Secret Key 변경 | 무작위 값으로 변경 | `GF_SECURITY_SECRET_KEY` |
| CSP 활성화 | `content_security_policy = true` | 응답 헤더 확인 |
| HSTS 활성화 | `strict_transport_security = true` | 응답 헤더 확인 |
| 쿠키 보안 | `cookie_secure = true` | HTTPS 환경에서만 |
| 임베딩 차단 | `allow_embedding = false` | X-Frame-Options 확인 |
| 로그 레벨 | 프로덕션: `info` 이상 | `GF_LOG_LEVEL` |
| DB SSL | `ssl_mode = require` | PostgreSQL/MySQL 연결 확인 |
| 미사용 인증 비활성화 | 사용하지 않는 OAuth 제공자 `enabled = false` | 설정 검토 |

---

## 요약

Grafana 운영의 핵심 요소:

| 영역 | 핵심 설정 | 프로덕션 권장값 |
|------|----------|---------------|
| 배포 | Docker/Helm | Kubernetes Helm Chart |
| DB | `[database]` | PostgreSQL + 커넥션 풀 |
| 인증 | `[auth.*]` | OAuth2/SAML + LDAP |
| 프로비저닝 | `conf/provisioning/` | GitOps YAML |
| 모니터링 | `/metrics`, `/api/health` | Prometheus + 알림 |
| 로그 | `[log]` | `mode=console`, `level=info` |
| 트레이싱 | `[tracing.opentelemetry.otlp]` | OTLP → Tempo |
| HA | 공유 DB + Redis | PostgreSQL + Redis |
| 보안 | `[security]` | HTTPS + CSP + HSTS |

설정 로딩 체인(`defaults.ini` → `grafana.ini` → 환경 변수)을 이해하면, 다양한 배포 환경에서 유연하게 Grafana를 구성할 수 있다. 프로비저닝을 활용하면 Infrastructure as Code 방식으로 데이터소스, 대시보드, 알림 규칙을 선언적으로 관리할 수 있다.
