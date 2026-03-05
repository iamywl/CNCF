# Helm v4 운영 가이드

## 1. 설치 방법

### 패키지 매니저

| 플랫폼 | 명령 |
|--------|------|
| macOS (Homebrew) | `brew install helm` |
| Windows (Chocolatey) | `choco install kubernetes-helm` |
| Windows (Scoop) | `scoop install helm` |
| Windows (Winget) | `winget install Helm.Helm` |
| Debian/Ubuntu | `sudo snap install helm --classic` |
| Fedora | `sudo dnf install helm` |

### 바이너리 직접 설치

```bash
# 공식 설치 스크립트
curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# 또는 릴리스 페이지에서 직접 다운로드
# https://github.com/helm/helm/releases
wget https://get.helm.sh/helm-v4.x.x-linux-amd64.tar.gz
tar -zxvf helm-v4.x.x-linux-amd64.tar.gz
sudo mv linux-amd64/helm /usr/local/bin/helm
```

### 소스에서 빌드

```bash
git clone https://github.com/helm/helm.git
cd helm
make build          # bin/helm 생성
make install        # /usr/local/bin/helm에 설치
```

빌드 시 CGO는 비활성화(`CGO_ENABLED=0`)되며, 정적 바이너리가 생성된다.

### 설치 확인

```bash
helm version
# version.BuildInfo{Version:"v4.x.x", GitCommit:"...", GitTreeState:"clean", GoVersion:"go1.25.0"}
```

## 2. 기본 사용법

`pkg/cmd/root.go`의 `newRootCmdWithConfig()`에 정의된 커맨드 트리 기반으로 정리한다.

### 2.1 차트 검색

```bash
# Artifact Hub에서 검색
helm search hub wordpress

# 로컬 리포지토리에서 검색
helm search repo nginx

# 개발 버전 포함 검색
helm search repo nginx --devel
```

### 2.2 차트 설치 (helm install)

```bash
# 기본 설치
helm install my-release bitnami/nginx

# 네임스페이스 지정
helm install my-release bitnami/nginx -n production

# 네임스페이스 자동 생성
helm install my-release bitnami/nginx -n production --create-namespace

# Values 파일 지정
helm install my-release bitnami/nginx -f values.yaml

# 개별 값 설정
helm install my-release bitnami/nginx --set replicaCount=3

# 복합 값 설정
helm install my-release bitnami/nginx \
  --set replicaCount=3 \
  --set-string annotations."kubernetes\.io/ingress\.class"=nginx \
  --set-file sslCert=cert.pem

# DryRun (클라이언트 사이드)
helm install my-release bitnami/nginx --dry-run=client

# DryRun (서버 사이드 — 실제 API 검증)
helm install my-release bitnami/nginx --dry-run=server

# 리소스 준비 대기 (기본: kstatus watcher)
helm install my-release bitnami/nginx --wait --timeout 5m

# 레거시 대기 전략 사용
helm install my-release bitnami/nginx --wait --wait-strategy=legacy

# 로컬 차트 설치
helm install my-release ./mychart

# OCI 레지스트리에서 설치
helm install my-release oci://registry.example.com/charts/myapp --version 1.0.0

# Server-Side Apply 활성화 (기본값)
helm install my-release bitnami/nginx --server-side-apply

# 실패 시 자동 롤백
helm install my-release bitnami/nginx --rollback-on-failure
```

### 2.3 차트 업그레이드 (helm upgrade)

```bash
# 기본 업그레이드
helm upgrade my-release bitnami/nginx

# 없으면 설치 (install-or-upgrade)
helm upgrade --install my-release bitnami/nginx

# 값 재설정 (이전 릴리스 값 무시)
helm upgrade my-release bitnami/nginx --reset-values

# 이전 값 유지하며 추가
helm upgrade my-release bitnami/nginx --reuse-values --set replicaCount=5

# 히스토리 제한
helm upgrade my-release bitnami/nginx --history-max 10

# 강제 업그레이드 (리소스 충돌 무시)
helm upgrade my-release bitnami/nginx --force
```

### 2.4 롤백 (helm rollback)

```bash
# 이전 리비전으로 롤백
helm rollback my-release 1

# 리비전 히스토리 확인 후 롤백
helm history my-release
helm rollback my-release 3

# 롤백 시 대기
helm rollback my-release 1 --wait --timeout 3m
```

### 2.5 삭제 (helm uninstall)

```bash
# 릴리스 삭제
helm uninstall my-release

# 히스토리 유지 (나중에 rollback 가능)
helm uninstall my-release --keep-history

# 네임스페이스 지정
helm uninstall my-release -n production

# 삭제 대기
helm uninstall my-release --wait --timeout 3m

# Cascade 삭제 정책 지정 (기본: background)
helm uninstall my-release --cascade=foreground
```

### 2.6 릴리스 조회

```bash
# 현재 네임스페이스의 릴리스 목록
helm list

# 모든 네임스페이스
helm list -A

# 상태별 필터링
helm list --deployed
helm list --failed
helm list --pending
helm list --uninstalled  # --keep-history로 삭제된 것

# JSON 출력
helm list -o json

# 릴리스 상태 확인
helm status my-release

# 릴리스 히스토리
helm history my-release

# 릴리스 상세 정보
helm get all my-release
helm get values my-release
helm get manifest my-release
helm get hooks my-release
helm get notes my-release
helm get metadata my-release
```

### 2.7 차트 관리

```bash
# 차트 미리보기
helm show chart bitnami/nginx
helm show values bitnami/nginx
helm show readme bitnami/nginx
helm show all bitnami/nginx

# 차트 다운로드
helm pull bitnami/nginx
helm pull bitnami/nginx --untar

# 차트 생성 (스캐폴딩)
helm create mychart

# 차트 린트
helm lint ./mychart

# 차트 패키징
helm package ./mychart

# 차트 검증 (서명)
helm verify mychart-0.1.0.tgz

# 차트 의존성 관리
helm dependency list ./mychart
helm dependency update ./mychart
helm dependency build ./mychart

# 템플릿 렌더링 (로컬)
helm template my-release ./mychart
helm template my-release ./mychart -f values.yaml --output-dir ./output
```

## 3. 환경변수 테이블

`pkg/cmd/root.go`의 `globalUsage`에 정의된 환경변수 목록이다.

### 핵심 환경변수

| 환경변수 | 기본값 | 설명 |
|---------|--------|------|
| `HELM_CACHE_HOME` | `$HOME/.cache/helm` (Linux) | 캐시 파일 저장 경로 |
| `HELM_CONFIG_HOME` | `$HOME/.config/helm` (Linux) | 설정 파일 저장 경로 |
| `HELM_DATA_HOME` | `$HOME/.local/share/helm` (Linux) | 데이터 파일 저장 경로 (플러그인 등) |
| `HELM_DEBUG` | `false` | 디버그 모드 활성화 |
| `HELM_DRIVER` | `secret` | 릴리스 스토리지 드라이버 (secret, configmap, memory, sql) |
| `HELM_DRIVER_SQL_CONNECTION_STRING` | (없음) | SQL 드라이버 연결 문자열 |
| `HELM_MAX_HISTORY` | `10` | 최대 릴리스 히스토리 수 |
| `HELM_NAMESPACE` | `default` | 기본 네임스페이스 |
| `HELM_NO_PLUGINS` | `0` | `1`로 설정 시 플러그인 비활성화 |

### 경로 환경변수

| 환경변수 | 기본값 | 설명 |
|---------|--------|------|
| `HELM_PLUGINS` | `$HELM_DATA_HOME/plugins` | 플러그인 디렉토리 |
| `HELM_REGISTRY_CONFIG` | `$HELM_CONFIG_HOME/registry/config.json` | 레지스트리 인증 설정 파일 |
| `HELM_REPOSITORY_CACHE` | `$HELM_CACHE_HOME/repository` | 리포지토리 인덱스 캐시 |
| `HELM_REPOSITORY_CONFIG` | `$HELM_CONFIG_HOME/repositories.yaml` | 리포지토리 목록 파일 |
| `HELM_CONTENT_CACHE` | `$HELM_CACHE_HOME/content` | 차트 콘텐츠 캐시 |

### Kubernetes 연결 환경변수

| 환경변수 | 기본값 | 설명 |
|---------|--------|------|
| `KUBECONFIG` | `~/.kube/config` | kubeconfig 파일 경로 |
| `HELM_KUBECONTEXT` | (없음) | kubeconfig 컨텍스트명 |
| `HELM_KUBETOKEN` | (없음) | Bearer 토큰 인증 |
| `HELM_KUBEASUSER` | (없음) | 사용자 위장 (impersonation) |
| `HELM_KUBEASGROUPS` | (없음) | 그룹 위장 (쉼표 구분) |
| `HELM_KUBEAPISERVER` | (없음) | API 서버 엔드포인트 |
| `HELM_KUBECAFILE` | (없음) | CA 인증서 파일 |
| `HELM_KUBEINSECURE_SKIP_TLS_VERIFY` | `false` | TLS 인증서 검증 건너뛰기 |
| `HELM_KUBETLS_SERVER_NAME` | (없음) | TLS 서버명 오버라이드 |

### 성능/UI 환경변수

| 환경변수 | 기본값 | 설명 |
|---------|--------|------|
| `HELM_BURST_LIMIT` | `100` | K8s API 클라이언트 버스트 제한 (-1로 비활성화) |
| `HELM_QPS` | `0` (라이브러리 기본값) | 초당 쿼리 수 제한 |
| `HELM_COLOR` | `auto` | 컬러 출력 모드 (never, auto, always) |
| `NO_COLOR` | (없음) | 비어있지 않으면 컬러 비활성화 (표준 규약) |

### OS별 기본 디렉토리

`pkg/cmd/root.go`의 `globalUsage`에서 참조하는 기본 경로:

| OS | 캐시 | 설정 | 데이터 |
|-----|------|------|------|
| **Linux** | `$HOME/.cache/helm` | `$HOME/.config/helm` | `$HOME/.local/share/helm` |
| **macOS** | `$HOME/Library/Caches/helm` | `$HOME/Library/Preferences/helm` | `$HOME/Library/helm` |
| **Windows** | `%TEMP%\helm` | `%APPDATA%\helm` | `%APPDATA%\helm` |

이 경로는 `pkg/helmpath/` 패키지에서 XDG Base Directory Specification을 따라 결정된다.

## 4. 스토리지 드라이버 설정

`pkg/action/action.go`의 `Configuration.Init()`에서 `HELM_DRIVER` 환경변수에 따라 드라이버를 선택한다.

### 4.1 Secret 드라이버 (기본값)

```bash
# 기본값 — 별도 설정 불필요
export HELM_DRIVER=secret
# 또는 설정하지 않으면 자동으로 secret 사용
```

릴리스 정보를 Kubernetes Secret에 저장한다. 데이터는 base64 + gzip으로 인코딩되어 Secret의 data 필드에 저장된다.

```bash
# 저장된 Secret 확인
kubectl get secret -l owner=helm,name=my-release
# 이름 형식: sh.helm.release.v1.my-release.v1
```

**장점:** RBAC으로 접근 제어 가능, etcd 암호화 적용 시 저장 시 암호화
**단점:** etcd 저장 크기 제한 (기본 1MB), 대규모 차트에서 문제 가능

### 4.2 ConfigMap 드라이버

```bash
export HELM_DRIVER=configmap
```

Secret 대신 ConfigMap에 저장한다. 동작은 Secret 드라이버와 동일하다.

**사용 시점:** Secret 접근 권한이 없는 환경

### 4.3 Memory 드라이버

```bash
export HELM_DRIVER=memory
export HELM_MEMORY_DRIVER_DATA=/path/to/releases.yaml
```

인메모리 저장소. 프로세스 종료 시 데이터가 사라진다.

**사용 시점:** 테스트, CI/CD 파이프라인, 임시 환경

`HELM_MEMORY_DRIVER_DATA` 환경변수로 초기 데이터를 YAML 파일에서 로딩할 수 있다 (콜론으로 여러 파일 구분).

### 4.4 SQL 드라이버

```bash
export HELM_DRIVER=sql
export HELM_DRIVER_SQL_CONNECTION_STRING="host=localhost port=5432 dbname=helm user=helm password=... sslmode=disable"
```

PostgreSQL에 릴리스를 저장한다. 대규모 환경에서 etcd 부하를 줄인다.

**장점:** 릴리스 크기 제한 없음, SQL 쿼리로 직접 조회 가능
**단점:** 별도 DB 인프라 필요, 추가 의존성

## 5. OCI 레지스트리 연동

Helm v4는 OCI(Open Container Initiative) 레지스트리를 차트 저장소로 사용한다.

### 5.1 레지스트리 로그인/로그아웃

```bash
# 로그인
helm registry login registry.example.com
# Username: admin
# Password: ****

# 사용자/패스워드 직접 지정
helm registry login registry.example.com -u admin -p password

# 자격 증명 파일 위치
# $HELM_CONFIG_HOME/registry/config.json (기본)
# Docker 자격 증명(~/.docker/config.json)도 fallback으로 사용

# 로그아웃
helm registry logout registry.example.com
```

### 5.2 차트 Push/Pull

```bash
# 차트 패키징
helm package ./mychart
# mychart-1.0.0.tgz

# OCI 레지스트리에 Push
helm push mychart-1.0.0.tgz oci://registry.example.com/charts

# OCI 레지스트리에서 Pull
helm pull oci://registry.example.com/charts/mychart --version 1.0.0

# Pull + Untar
helm pull oci://registry.example.com/charts/mychart --version 1.0.0 --untar

# OCI에서 직접 설치
helm install my-release oci://registry.example.com/charts/mychart --version 1.0.0

# 태그 목록 조회
helm show chart oci://registry.example.com/charts/mychart
```

### 5.3 OCI 아티팩트 구조

```
OCI Image Manifest
├── Config Layer   (application/vnd.cncf.helm.config.v1+json)
│   └── Chart.yaml 메타데이터 (JSON)
├── Chart Layer    (application/vnd.cncf.helm.chart.content.v1.tar+gzip)
│   └── 차트 아카이브 (.tgz)
└── Prov Layer     (application/vnd.cncf.helm.chart.provenance.v1.prov)  [선택]
    └── 서명 파일
```

### 5.4 지원 레지스트리

| 레지스트리 | 지원 |
|-----------|------|
| Docker Hub | O |
| GitHub Container Registry (ghcr.io) | O |
| Amazon ECR | O |
| Google Artifact Registry | O |
| Azure Container Registry | O |
| Harbor | O |
| Quay.io | O |
| GitLab Container Registry | O |

## 6. 리포지토리 관리

### 6.1 리포지토리 추가/관리

```bash
# 리포지토리 추가
helm repo add bitnami https://charts.bitnami.com/bitnami

# 인증이 필요한 리포지토리
helm repo add my-repo https://charts.example.com \
  --username admin --password secret

# CA 인증서 지정
helm repo add my-repo https://charts.example.com \
  --ca-file ca.crt --cert-file client.crt --key-file client.key

# 기존 리포지토리 URL 변경
helm repo add bitnami https://charts.bitnami.com/bitnami --force-update

# 리포지토리 인덱스 업데이트
helm repo update

# 특정 리포지토리만 업데이트
helm repo update bitnami

# 리포지토리 목록 조회
helm repo list

# 리포지토리 제거
helm repo remove bitnami

# 리포지토리 인덱스 생성 (차트 호스팅 시)
helm repo index ./charts-dir --url https://charts.example.com
```

### 6.2 리포지토리 설정 파일

```bash
# 위치: $HELM_REPOSITORY_CONFIG (기본: $HELM_CONFIG_HOME/repositories.yaml)
cat ~/.config/helm/repositories.yaml
```

```yaml
apiVersion: ""
generated: "2024-01-01T00:00:00Z"
repositories:
- name: bitnami
  url: https://charts.bitnami.com/bitnami
  caFile: ""
  certFile: ""
  keyFile: ""
  insecure_skip_tls_verify: false
  pass_credentials_all: false
```

## 7. 플러그인 관리

### 7.1 플러그인 명령

```bash
# 플러그인 설치
helm plugin install https://github.com/databus23/helm-diff

# 특정 버전 설치
helm plugin install https://github.com/databus23/helm-diff --version v3.9.0

# 플러그인 목록
helm plugin list

# 플러그인 업데이트
helm plugin update diff

# 플러그인 삭제
helm plugin uninstall diff
```

### 7.2 Helm v4 플러그인 시스템

Helm v4는 기존 exec 기반 플러그인에 추가로 WASM 런타임을 도입했다:

| 런타임 | 의존성 | 설명 |
|--------|--------|------|
| exec | 없음 | 기존 방식 -- 외부 프로세스 실행 |
| WASM (wazero) | `github.com/tetratelabs/wazero` | 순수 Go WASM 런타임, CGO 불필요 |
| WASM (Extism) | `github.com/extism/go-sdk` | Extism SDK 기반 WASM 플러그인 |

### 7.3 플러그인 디렉토리

```bash
# 기본 경로
echo $HELM_PLUGINS
# $HELM_DATA_HOME/plugins (예: ~/.local/share/helm/plugins)

# 플러그인 구조
$HELM_PLUGINS/
└── helm-diff/
    ├── plugin.yaml      # 플러그인 메타데이터
    ├── bin/
    │   └── diff          # 실행 바이너리
    └── ...
```

### 7.4 포스트 렌더러 플러그인

Helm v4는 포스트 렌더러를 플러그인 시스템으로 통합했다:

```bash
# Kustomize 포스트 렌더러 사용
helm install my-release ./mychart --post-renderer kustomize

# 커스텀 포스트 렌더러 플러그인
helm install my-release ./mychart --post-renderer my-postrenderer
```

포스트 렌더러는 렌더링된 매니페스트를 stdin으로 받아 수정된 매니페스트를 stdout으로 출력한다.

## 8. 트러블슈팅

### 8.1 일반 문제

| 증상 | 원인 | 해결 |
|------|------|------|
| `Error: INSTALLATION FAILED: cannot re-use a name that is still in use` | 동일 이름 릴리스 존재 | `helm list -A`로 확인 후 삭제 또는 다른 이름 사용 |
| `Error: UPGRADE FAILED: another operation (install/upgrade/rollback) is in progress` | 이전 작업이 중단됨 | `helm rollback <release> <revision>` 또는 `helm uninstall` |
| `Error: release: not found` | 릴리스 존재하지 않음 | `helm list -A`로 네임스페이스 확인 |
| `Error: chart requires kubeVersion: >=1.25 which is incompatible` | K8s 버전 불일치 | K8s 업그레이드 또는 차트 다운그레이드 |
| 릴리스가 `pending-install` 상태로 멈춤 | 설치 중 프로세스 중단 | `helm rollback <release> 0` 또는 `helm uninstall <release>` |
| `Error: rendered manifests contain a resource that already exists` | 리소스 충돌 | `--force` 사용 또는 기존 리소스 삭제 |

### 8.2 스토리지 문제

| 증상 | 원인 | 해결 |
|------|------|------|
| `etcdserver: request is too large` | 차트가 너무 커서 Secret/ConfigMap 크기 초과 | SQL 드라이버 사용 또는 차트 최적화 |
| Secret이 너무 많음 | MaxHistory 미설정 | `--history-max` 설정 (기본값 v4: 10) |
| `HELM_DRIVER` 변경 후 릴리스 못 찾음 | 드라이버 변경 시 기존 데이터 미이전 | 원래 드라이버로 복원 후 데이터 마이그레이션 |

### 8.3 네트워크/인증 문제

| 증상 | 원인 | 해결 |
|------|------|------|
| `x509: certificate signed by unknown authority` | CA 인증서 미등록 | `--ca-file` 또는 `HELM_KUBECAFILE` 설정 |
| `unable to connect to the server` | API 서버 연결 불가 | `kubectl cluster-info`로 확인, kubeconfig 점검 |
| OCI 레지스트리 인증 실패 | 자격 증명 만료/없음 | `helm registry login`으로 재인증 |
| `insecure skip TLS verify` 경고 | 자체 서명 인증서 사용 | CA 인증서 등록 또는 `--insecure-skip-tls-verify` |

### 8.4 디버그 모드

```bash
# 디버그 출력 활성화
helm install my-release ./mychart --debug

# 환경변수로 전역 활성화
export HELM_DEBUG=true

# 디버그 출력 내용:
# - 렌더링된 매니페스트 전체 출력
# - K8s API 요청/응답
# - 값 병합 과정
# - 드라이버 작업 로그
```

### 8.5 유용한 디버그 명령

```bash
# 렌더링 결과만 확인 (클러스터 연결 없이)
helm template my-release ./mychart -f values.yaml --debug

# 값 병합 결과 확인
helm get values my-release -o yaml

# 현재 매니페스트 확인
helm get manifest my-release

# Helm 환경 변수 확인
helm env

# 릴리스 상태 상세 확인
helm status my-release --show-desc --show-resources
```

## 9. CI/CD 통합

### 9.1 환경 변수 기반 설정

CI/CD 환경에서는 환경 변수로 Helm을 제어한다:

```bash
# CI/CD 파이프라인 환경 변수 설정
export KUBECONFIG=/path/to/kubeconfig
export HELM_NAMESPACE=production
export HELM_MAX_HISTORY=5
export HELM_DRIVER=secret
export HELM_DEBUG=false

# 컬러 출력 비활성화 (CI 로그용)
export NO_COLOR=1
# 또는
export HELM_COLOR=never
```

### 9.2 GitHub Actions 예시

```yaml
name: Helm Deploy
on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v4

    - name: Install Helm
      uses: azure/setup-helm@v3

    - name: Configure kubectl
      uses: azure/k8s-set-context@v3
      with:
        kubeconfig: ${{ secrets.KUBECONFIG }}

    - name: Lint Chart
      run: helm lint ./charts/my-app

    - name: Deploy
      run: |
        helm upgrade --install my-app ./charts/my-app \
          --namespace production \
          --create-namespace \
          --wait \
          --timeout 10m \
          --history-max 5 \
          --set image.tag=${{ github.sha }} \
          -f values-production.yaml
```

### 9.3 GitLab CI 예시

```yaml
deploy:
  stage: deploy
  image: alpine/helm:latest
  variables:
    HELM_NAMESPACE: production
    HELM_MAX_HISTORY: "5"
    NO_COLOR: "1"
  script:
    - helm upgrade --install my-app ./charts/my-app
        --wait
        --timeout 10m
        --set image.tag=${CI_COMMIT_SHA}
        -f values-production.yaml
```

### 9.4 Helmfile을 사용한 선언적 관리

```yaml
# helmfile.yaml
releases:
  - name: my-app
    namespace: production
    chart: ./charts/my-app
    values:
      - values-production.yaml
    set:
      - name: image.tag
        value: "{{ requiredEnv "IMAGE_TAG" }}"
    wait: true
    timeout: 600
    historyMax: 5
```

### 9.5 CI/CD 베스트 프랙티스

| 항목 | 권장 |
|------|------|
| **DryRun 검증** | PR 단계에서 `helm template` + `helm lint` 실행 |
| **히스토리 제한** | `--history-max 5~10` -- Secret 누적 방지 |
| **타임아웃 설정** | `--timeout 5m~15m` -- 무한 대기 방지 |
| **대기 활성화** | `--wait` -- 배포 완료 확인 후 다음 단계 진행 |
| **네임스페이스 명시** | `-n production` -- 실수 방지 |
| **값 파일 분리** | 환경별 `values-{env}.yaml` 분리 관리 |
| **이미지 태그** | `--set image.tag=$SHA` -- 재현 가능한 배포 |
| **시크릿 관리** | `--set-file`으로 시크릿 파일 주입, 차트에 하드코딩 금지 |
| **컬러 비활성화** | `NO_COLOR=1` -- CI 로그 가독성 확보 |

## 10. 모니터링/관찰

### 10.1 릴리스 상태 모니터링

```bash
# 릴리스 상태 상수 (pkg/release/common/status.go)
# StatusDeployed    = "deployed"
# StatusUninstalled = "uninstalled"
# StatusSuperseded  = "superseded"
# StatusFailed      = "failed"
# StatusUninstalling = "uninstalling"
# StatusPendingInstall  = "pending-install"
# StatusPendingUpgrade  = "pending-upgrade"
# StatusPendingRollback = "pending-rollback"

# 전체 릴리스 상태 확인
helm list -A -o json | jq '.[] | {name, namespace, status, updated}'

# 실패한 릴리스 찾기
helm list -A --failed
```

### 10.2 Kubernetes 이벤트 모니터링

```bash
# Helm이 생성한 리소스의 이벤트 확인
kubectl get events -n production --sort-by='.lastTimestamp'

# Helm Secret 모니터링
kubectl get secret -l owner=helm -n production -w
```

## 11. 보안 고려사항

### 11.1 RBAC 최소 권한

```yaml
# Helm 서비스 계정에 필요한 최소 권한
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: helm-deployer
  namespace: production
rules:
# Secret 드라이버용
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "create", "update", "delete"]
  # 레이블 셀렉터: owner=helm
# 차트 리소스 관리
- apiGroups: ["", "apps", "batch"]
  resources: ["*"]
  verbs: ["*"]
```

### 11.2 보안 베스트 프랙티스

| 항목 | 설명 |
|------|------|
| **TLS 검증** | `--kube-insecure-skip-tls-verify` 사용 금지 (프로덕션) |
| **Secret 드라이버** | 기본 Secret 드라이버 사용 -- ConfigMap은 base64 인코딩만 |
| **etcd 암호화** | Secret 드라이버 + etcd at-rest encryption 조합 |
| **서명 검증** | `helm verify`로 차트 무결성 확인 |
| **OCI 레지스트리** | 프라이빗 레지스트리 + TLS 사용 |
| **네임스페이스 격리** | 환경별 네임스페이스 분리 + RBAC |
| **히스토리 정리** | `--history-max`로 민감 데이터 노출 최소화 |
