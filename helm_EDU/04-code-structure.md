# Helm v4 코드 구조

## 1. 전체 디렉토리 트리

```
helm/
├── cmd/
│   └── helm/                          # CLI 진입점 (main.go)
│       └── helm.go                    # main() 함수 — 프로그램 시작점
├── internal/                          # 비공개 내부 패키지 (외부 임포트 불가)
│   ├── chart/
│   │   └── v3/                        # v3(apiVersion: v3) 차트 내부 구현
│   ├── cli/
│   │   └── output/                    # CLI 출력 포맷팅 (내부용)
│   ├── copystructure/                 # 구조체 딥카피 유틸
│   ├── fileutil/                      # 파일 유틸리티
│   ├── gates/                         # Feature gate 관리
│   ├── logging/                       # 로깅 인프라 (LogHolder 등)
│   ├── monocular/                     # Monocular 검색 통합
│   ├── plugin/                        # 플러그인 시스템 내부 구현
│   │   ├── cache/                     #   플러그인 캐시
│   │   ├── installer/                 #   플러그인 설치기
│   │   ├── schema/                    #   플러그인 스키마 정의
│   │   └── testdata/                  #   테스트 데이터
│   ├── release/
│   │   └── v2/                        # v2 릴리스 내부 구현 (향후 버전)
│   ├── resolver/                      # 의존성 해석기
│   ├── statusreaders/                 # 리소스 상태 읽기 (kstatus 통합)
│   ├── sympath/                       # 심볼릭 링크 경로 처리
│   ├── test/
│   │   └── ensure/                    # 테스트 헬퍼
│   ├── third_party/                   # 서드파티 코드
│   │   ├── dep/                       #   의존성 잠금 파일
│   │   └── k8s.io/                    #   Kubernetes 유틸
│   ├── tlsutil/                       # TLS 설정 유틸리티
│   ├── urlutil/                       # URL 파싱 유틸리티
│   └── version/                       # 버전 정보 (빌드 시 주입)
├── pkg/                               # 공개 API 패키지 (외부 임포트 가능)
│   ├── action/                        # 핵심 액션 로직 (install, upgrade, rollback 등)
│   ├── chart/                         # 차트 인터페이스 및 로더
│   │   ├── common/                    #   공통 타입 (File, Capabilities, Values)
│   │   ├── loader/                    #   차트 로딩 (디렉토리/아카이브 → Charter)
│   │   └── v2/                        #   v2(apiVersion: v1/v2) 차트 구현
│   ├── cli/                           # CLI 환경 설정
│   │   ├── output/                    #   출력 포맷팅 (table, json, yaml)
│   │   └── values/                    #   Values 병합 로직
│   ├── cmd/                           # Cobra 커맨드 정의
│   │   ├── require/                   #   인수 검증
│   │   └── search/                    #   검색 기능
│   ├── downloader/                    # 차트 다운로드 및 캐시
│   ├── engine/                        # Go 템플릿 렌더링 엔진
│   ├── gates/                         # Feature gate 공개 API
│   ├── getter/                        # URL 스킴별 다운로더 (http, oci 등)
│   ├── helmpath/                      # Helm 경로 관리 (XDG 규격)
│   │   └── xdg/                       #   XDG 디렉토리 구현
│   ├── ignore/                        # .helmignore 파싱
│   ├── kube/                          # Kubernetes 클라이언트 래퍼
│   │   └── fake/                      #   테스트용 Fake 클라이언트
│   ├── postrenderer/                  # 포스트 렌더링 인터페이스 및 플러그인
│   ├── provenance/                    # 차트 서명/검증
│   ├── pusher/                        # 차트 업로드
│   ├── registry/                      # OCI 레지스트리 클라이언트
│   ├── release/                       # 릴리스 인터페이스 및 타입
│   │   ├── common/                    #   공통 상태 상수 (Deployed, Failed 등)
│   │   └── v1/                        #   v1 릴리스 구현체
│   ├── repo/                          # 차트 리포지토리 관리
│   │   └── v1/                        #   v1 리포지토리 구현
│   ├── storage/                       # 릴리스 스토리지 추상화
│   │   └── driver/                    #   스토리지 드라이버 (Secret, ConfigMap, Memory, SQL)
│   ├── strvals/                       # --set 플래그 값 파싱
│   └── uploader/                      # 차트 업로더
├── scripts/                           # 빌드/릴리스 스크립트
├── testdata/                          # 통합 테스트 데이터
├── .github/                           # GitHub Actions 워크플로우
├── go.mod                             # Go 모듈 정의
├── go.sum                             # 의존성 체크섬
└── Makefile                           # 빌드 시스템
```

## 2. cmd/ vs pkg/ vs internal/ 역할 구분

Helm v4는 Go 표준 프로젝트 레이아웃을 따른다.

### cmd/ -- 프로그램 진입점

```
cmd/helm/helm.go
```

`main()` 함수만 포함한다. 실제 로직은 `pkg/cmd.NewRootCmd()`에 위임한다.

```go
// cmd/helm/helm.go
func main() {
    kube.ManagedFieldsManager = "helm"
    cmd, err := helmcmd.NewRootCmd(os.Stdout, os.Args[1:], helmcmd.SetupLogging)
    if err != nil {
        slog.Warn("command failed", slog.Any("error", err))
        os.Exit(1)
    }
    if err := cmd.Execute(); err != nil {
        var cerr helmcmd.CommandError
        if errors.As(err, &cerr) {
            os.Exit(cerr.ExitCode)
        }
        os.Exit(1)
    }
}
```

핵심 설계: `cmd/helm/helm.go`는 순수한 부트스트래퍼이다. Cobra 커맨드 트리 생성, 로깅 초기화, 에러 코드 처리만 담당한다.

### pkg/ -- 공개 API

외부 프로젝트(Flux, Argo CD, Terraform Provider 등)가 Helm을 라이브러리로 사용할 때 임포트하는 패키지이다.

| 계층 | 대표 패키지 | 역할 |
|------|-----------|------|
| **진입 계층** | `pkg/cmd` | Cobra 커맨드 정의, CLI 플래그 바인딩 |
| **액션 계층** | `pkg/action` | install, upgrade, rollback 등 비즈니스 로직 |
| **엔진 계층** | `pkg/engine` | Go template + Sprig 렌더링 |
| **스토리지 계층** | `pkg/storage`, `pkg/storage/driver` | 릴리스 CRUD, 드라이버 추상화 |
| **K8s 계층** | `pkg/kube` | Kubernetes 리소스 CRUD, Wait/Watch |
| **차트 계층** | `pkg/chart`, `pkg/chart/loader` | 차트 타입 정의, 로딩 |
| **레지스트리 계층** | `pkg/registry` | OCI Push/Pull/Login/Logout |
| **유틸 계층** | `pkg/getter`, `pkg/helmpath`, `pkg/strvals` | URL 다운로드, 경로 관리, 값 파싱 |

### internal/ -- 비공개 패키지

Go의 `internal/` 규칙에 의해 이 모듈 외부에서는 임포트할 수 없다. 구현 세부사항을 숨긴다.

| 패키지 | 역할 |
|--------|------|
| `internal/chart/v3` | v3 차트 포맷(apiVersion: v3) 구현체 -- v4에서 새로 도입 |
| `internal/release/v2` | v2 릴리스 포맷 -- 향후 릴리스 객체 진화를 위한 준비 |
| `internal/plugin` | 플러그인 런타임(WASM/exec), 설치기, 캐시 |
| `internal/logging` | `LogHolder` 임베딩 기반 구조화 로깅 |
| `internal/version` | 빌드 시 `-ldflags`로 주입되는 버전 메타데이터 |
| `internal/statusreaders` | kstatus 기반 리소스 상태 감시 |
| `internal/resolver` | 차트 의존성 해석 알고리즘 |
| `internal/tlsutil` | TLS 설정 빌더 |
| `internal/sympath` | 심볼릭 링크 안전 경로 순회 |

## 3. pkg 패키지별 책임 매핑 테이블

| 패키지 | 파일 수 | 핵심 타입/함수 | 책임 |
|--------|---------|--------------|------|
| `pkg/action` | ~25개 | `Configuration`, `Install`, `Upgrade`, `Rollback`, `Uninstall`, `List`, `History` | 모든 Helm 명령의 비즈니스 로직. `Configuration`이 DI 컨테이너 역할 |
| `pkg/chart` | 인터페이스 | `Charter` (any), `Accessor`, `Dependency` | 차트 버전 독립적인 인터페이스 정의 |
| `pkg/chart/common` | 공통 | `File`, `Capabilities`, `Values`, `KubeVersion` | 차트 버전 간 공유 타입 |
| `pkg/chart/v2` | v2 구현 | `Chart`, `Metadata` | apiVersion v1/v2 차트 구현체 (Helm 3 호환) |
| `pkg/chart/loader` | 2개 | `Load()`, `LoadDir()`, `LoadFile()`, `LoadArchive()` | 디렉토리/아카이브에서 Charter 로딩, apiVersion에 따라 v2/v3 분기 |
| `pkg/cli` | 1개 | `EnvSettings` | 환경 변수, CLI 플래그 통합 관리 |
| `pkg/cli/values` | 1개 | `Options` | `-f`, `--set`, `--set-string`, `--set-file` 값 병합 |
| `pkg/cmd` | 다수 | `NewRootCmd()`, 각 서브커맨드 | Cobra 커맨드 트리 정의 |
| `pkg/downloader` | 3개 | `ChartDownloader`, `Manager` | 차트 다운로드, 의존성 업데이트, 캐시 관리 |
| `pkg/engine` | 3개 | `Engine`, `Render()`, `funcMap()` | Go template + Sprig + Helm 커스텀 함수 렌더링 |
| `pkg/gates` | 1개 | `Gate` | 환경변수 기반 Feature Gate |
| `pkg/getter` | 4개 | `Getter` 인터페이스, `HTTPGetter`, `OCIGetter`, `PluginGetter` | URL 스킴별 콘텐츠 다운로드 |
| `pkg/helmpath` | 2개 | `CachePath()`, `ConfigPath()`, `DataPath()` | XDG 규격 기반 Helm 디렉토리 경로 |
| `pkg/ignore` | 1개 | `Rules` | `.helmignore` 파싱 및 매칭 |
| `pkg/kube` | 다수 | `Client`, `Interface`, `Waiter` | Kubernetes API CRUD, Wait/Watch |
| `pkg/postrenderer` | 1개 | `PostRenderer` 인터페이스 | 렌더링 후 매니페스트 변환 (Kustomize, 플러그인) |
| `pkg/provenance` | 1개 | `Verify()` | 차트 서명 검증 (PGP) |
| `pkg/pusher` | 1개 | | 차트 업로드 |
| `pkg/registry` | 다수 | `Client`, `Push()`, `Pull()`, `Login()`, `Tags()` | OCI 레지스트리 연동 |
| `pkg/release` | 인터페이스 | `Releaser` (any), `Accessor`, `Hook` | 릴리스 버전 독립적인 인터페이스 |
| `pkg/release/v1` | v1 구현 | `Release`, `Hook` | v1 릴리스 구현체 (Helm 3 호환) |
| `pkg/repo` | 리포지토리 | | 차트 리포지토리 인덱스 관리 |
| `pkg/storage` | 1개 | `Storage`, `Init()`, `Get()`, `Create()`, `Update()`, `Delete()`, `History()`, `Last()` | 릴리스 스토리지 CRUD + 히스토리 관리 |
| `pkg/storage/driver` | 5개 | `Driver` 인터페이스, `Secrets`, `ConfigMaps`, `Memory`, `SQL` | 스토리지 백엔드 구현체 |
| `pkg/strvals` | 1개 | `Parse()` | `--set key=value` 문자열 파싱 → map 변환 |
| `pkg/uploader` | 1개 | | 차트 업로드 |

## 4. 핵심 의존성 목록

`go.mod`의 모듈명은 `helm.sh/helm/v4`이며, Go 1.25.0을 사용한다.

### 직접 의존성 (require)

| 의존성 | 버전 | 용도 |
|--------|------|------|
| `github.com/spf13/cobra` | v1.10.2 | CLI 프레임워크 (커맨드 트리, 플래그, 자동완성) |
| `github.com/spf13/pflag` | v1.0.10 | POSIX 호환 플래그 파싱 |
| `k8s.io/client-go` | v0.35.1 | Kubernetes API 클라이언트 |
| `k8s.io/cli-runtime` | v0.35.1 | kubectl 팩토리, RESTClientGetter |
| `k8s.io/apimachinery` | v0.35.1 | Kubernetes 타입 시스템 |
| `k8s.io/api` | v0.35.1 | Kubernetes API 객체 |
| `k8s.io/kubectl` | v0.35.1 | kubectl 유틸리티 |
| `oras.land/oras-go/v2` | v2.6.0 | OCI 레지스트리 클라이언트 (Push/Pull) |
| `github.com/Masterminds/sprig/v3` | v3.3.0 | 템플릿 함수 라이브러리 (160+ 함수) |
| `github.com/Masterminds/semver/v3` | v3.4.0 | 시맨틱 버전 파싱/비교 |
| `github.com/BurntSushi/toml` | v1.6.0 | TOML 변환 (toToml/fromToml 함수) |
| `go.yaml.in/yaml/v3` | v3.0.4 | YAML 파싱/생성 |
| `sigs.k8s.io/yaml` | v1.6.0 | Kubernetes 스타일 YAML 처리 |
| `sigs.k8s.io/kustomize/kyaml` | v0.21.1 | YAML 노드 조작 (포스트 렌더링) |
| `sigs.k8s.io/controller-runtime` | v0.23.1 | GVK 감지, 스키마 유틸 |
| `github.com/fluxcd/cli-utils` | v0.37.1-flux.1 | kstatus 기반 리소스 상태 감시 |
| `github.com/distribution/distribution/v3` | v3.0.0 | OCI 레지스트리 레퍼런스 |
| `github.com/opencontainers/image-spec` | v1.1.1 | OCI 이미지 스펙 (매니페스트, 디스크립터) |
| `github.com/Masterminds/squirrel` | v1.5.4 | SQL 쿼리 빌더 (SQL 드라이버) |
| `github.com/jmoiron/sqlx` | v1.4.0 | SQL 확장 (SQL 드라이버) |
| `github.com/lib/pq` | v1.11.2 | PostgreSQL 드라이버 |
| `github.com/rubenv/sql-migrate` | v1.8.1 | SQL 마이그레이션 |
| `github.com/extism/go-sdk` | v1.7.1 | WASM 플러그인 런타임 (Extism) |
| `github.com/tetratelabs/wazero` | v1.11.0 | WASM 런타임 (wazero) |
| `github.com/ProtonMail/go-crypto` | v1.3.0 | PGP 서명/검증 |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.2 | JSON Schema 검증 (values.schema.json) |
| `github.com/evanphx/json-patch/v5` | v5.9.11 | JSON Patch (3-way merge) |
| `github.com/gobwas/glob` | v0.2.3 | Glob 패턴 매칭 |
| `github.com/gofrs/flock` | v0.13.0 | 파일 잠금 (동시 접근 방지) |
| `github.com/gosuri/uitable` | v0.0.4 | 터미널 테이블 출력 |
| `github.com/fatih/color` | v1.18.0 | 컬러 터미널 출력 |
| `github.com/cyphar/filepath-securejoin` | v0.6.1 | 경로 탈출 방지 (보안) |
| `github.com/Masterminds/vcs` | v1.13.3 | VCS 기반 플러그인 설치 |

### Helm v4 신규 의존성 (v3 대비)

| 의존성 | 용도 | v3와의 차이 |
|--------|------|------------|
| `github.com/extism/go-sdk` | WASM 플러그인 런타임 | v3에 없음 -- v4에서 WASM 기반 플러그인 지원 |
| `github.com/tetratelabs/wazero` | 순수 Go WASM 런타임 | v3에 없음 -- CGO 없이 WASM 실행 |
| `github.com/fluxcd/cli-utils` | kstatus 기반 상태 감시 | v3의 legacy polling을 kstatus watcher로 대체 |
| `sigs.k8s.io/kustomize/kyaml` | YAML 노드 조작 | v3에 없음 -- 포스트 렌더링 개선 |
| `sigs.k8s.io/controller-runtime` | GVK 유틸 | v3에 없음 |

## 5. 빌드 시스템

### Makefile 주요 타겟

```
helm/Makefile
```

| 타겟 | 명령 | 설명 |
|------|------|------|
| `all` (기본) | `build` | 기본 빌드 |
| `build` | `go build -trimpath -ldflags ... -o bin/helm ./cmd/helm` | CGO 비활성화 빌드 |
| `install` | `install bin/helm /usr/local/bin/helm` | 로컬 설치 |
| `test` | `test-style` + `test-unit` | 린트 + 단위 테스트 |
| `test-unit` | `go test -shuffle=on -count=1 -race -v ./...` | 레이스 검출 포함 단위 테스트 |
| `test-coverage` | `./scripts/coverage.sh` | 커버리지 리포트 |
| `test-style` | `golangci-lint run ./...` | 코드 스타일 검사 |
| `build-cross` | `gox -parallel=3 -osarch=...` | 크로스 컴파일 (10+ 플랫폼) |
| `dist` | tar/zip 패키징 | 배포 아카이브 생성 |
| `format` | `goimports -w -local helm.sh/helm` | 코드 포맷팅 |
| `gen-test-golden` | golden 파일 갱신 | 테스트 기대값 업데이트 |
| `clean` | `rm -rf bin/ _dist/` | 빌드 산출물 정리 |
| `tidy` | `go mod tidy` | 의존성 정리 |

### 빌드 변수 주입 (-ldflags)

```makefile
LDFLAGS += -X helm.sh/helm/v4/internal/version.version=${BINARY_VERSION}
LDFLAGS += -X helm.sh/helm/v4/internal/version.metadata=${VERSION_METADATA}
LDFLAGS += -X helm.sh/helm/v4/internal/version.gitCommit=${GIT_COMMIT}
LDFLAGS += -X helm.sh/helm/v4/internal/version.gitTreeState=${GIT_DIRTY}
```

빌드 시 `internal/version` 패키지의 변수를 링커 플래그로 설정한다. 이를 통해 `helm version` 명령이 정확한 빌드 정보를 출력한다.

### 크로스 컴파일 지원 플랫폼

```
darwin/amd64    darwin/arm64
linux/amd64     linux/386       linux/arm      linux/arm64
linux/loong64   linux/ppc64le   linux/s390x    linux/riscv64
windows/amd64   windows/arm64
```

12개 OS/아키텍처 조합을 지원하며, `CGO_ENABLED=0`으로 정적 바이너리를 생성한다.

## 6. v2/v4 버전 전환 구조

Helm v4는 차트와 릴리스의 버전 독립적 인터페이스를 도입하여, 여러 차트 포맷과 릴리스 포맷을 동시에 지원한다.

### 차트 버전 아키텍처

```
pkg/chart/
├── interfaces.go              # Charter (any), Accessor 인터페이스
├── common/                    # 공통 타입 (File, Values, Capabilities)
├── loader/
│   └── load.go                # apiVersion 감지 → v2 또는 v3 로더 분기
└── v2/                        # apiVersion v1/v2 차트 구현 (Helm 3 호환)

internal/chart/
└── v3/                        # apiVersion v3 차트 구현 (Helm 4 신규)
```

**로딩 흐름:**

```
Load(name) → Chart.yaml 읽기 → apiVersion 감지
                                ├── "v1" / "v2" / "" → c2load.Load()  (pkg/chart/v2)
                                └── "v3"             → c3load.Load()  (internal/chart/v3)
```

`pkg/chart/loader/load.go`의 핵심 분기 코드:

```go
// pkg/chart/loader/load.go
switch c.APIVersion {
case c2.APIVersionV1, c2.APIVersionV2, "":
    return c2load.Load(dir)
case c3.APIVersionV3:
    return c3load.Load(dir)
default:
    return nil, errors.New("unsupported chart version")
}
```

### 릴리스 버전 아키텍처

```
pkg/release/
├── interfaces.go              # Releaser (any), Accessor 인터페이스
├── common/                    # 공통 상태 상수 (StatusDeployed 등)
└── v1/                        # v1 릴리스 구현 (Helm 3 호환)

internal/release/
└── v2/                        # v2 릴리스 구현 (향후 사용)
```

### 인터페이스 기반 다형성

**Charter 인터페이스:**

```go
// pkg/chart/interfaces.go
type Charter any

type Accessor interface {
    Name() string
    IsRoot() bool
    MetadataAsMap() map[string]any
    Files() []*common.File
    Templates() []*common.File
    ChartFullPath() string
    IsLibraryChart() bool
    Dependencies() []Charter
    MetaDependencies() []Dependency
    Values() map[string]any
    Schema() []byte
    Deprecated() bool
}
```

**Releaser 인터페이스:**

```go
// pkg/release/interfaces.go
type Releaser any

type Accessor interface {
    Name() string
    Namespace() string
    Version() int
    Hooks() []Hook
    Manifest() string
    Notes() string
    Labels() map[string]string
    Chart() chart.Charter
    Status() string
    ApplyMethod() string
    DeployedAt() time.Time
}
```

이 설계의 핵심: `Charter`와 `Releaser`를 `any` 타입으로 선언하고, `Accessor` 인터페이스를 통해 접근한다. 새로운 차트/릴리스 버전을 추가할 때 기존 코드를 변경하지 않고 새 구현체만 추가하면 된다 (Open/Closed Principle).

### 버전 매핑

| 차트 apiVersion | 로더 패키지 | 릴리스 버전 | 비고 |
|----------------|-----------|-----------|------|
| `v1` | `pkg/chart/v2/loader` | `pkg/release/v1` | Helm 2/3 호환 (v1 차트) |
| `v2` | `pkg/chart/v2/loader` | `pkg/release/v1` | Helm 3 차트 (dependencies 등) |
| `v3` | `internal/chart/v3/loader` | `pkg/release/v1` | Helm 4 신규 차트 포맷 |

## 7. 계층 의존성 다이어그램

```
                    ┌──────────────┐
                    │  cmd/helm    │   main() 진입점
                    └──────┬───────┘
                           │
                    ┌──────▼───────┐
                    │   pkg/cmd    │   Cobra 커맨드 트리
                    └──────┬───────┘
                           │
              ┌────────────▼────────────┐
              │      pkg/action         │   비즈니스 로직 (Install, Upgrade, ...)
              └─┬──────┬──────┬──────┬──┘
                │      │      │      │
        ┌───────▼──┐ ┌─▼────┐│  ┌───▼──────┐
        │pkg/engine│ │ pkg/ ││  │pkg/kube   │
        │(렌더링)  │ │storage││  │(K8s CRUD) │
        └──────────┘ └──┬───┘│  └───────────┘
                        │    │
                   ┌────▼────▼────┐
                   │ pkg/storage  │
                   │   /driver    │
                   └──────────────┘
                   Secret│ConfigMap│Memory│SQL

        ┌──────────┐  ┌──────────┐  ┌───────────┐
        │pkg/chart │  │  pkg/    │  │   pkg/    │
        │ /loader  │  │ registry │  │ getter    │
        │(차트로딩)│  │(OCI연동) │  │(URL다운)  │
        └──────────┘  └──────────┘  └───────────┘
```

## 8. 테스트 구조

| 테스트 유형 | 위치 | 실행 |
|------------|------|------|
| 단위 테스트 | 각 패키지 내 `*_test.go` | `make test-unit` |
| Golden 테스트 | `pkg/action/testdata/`, `pkg/cmd/testdata/` | `make gen-test-golden` |
| 수락 테스트 | 별도 리포지토리 (`acceptance-testing`) | `make test-acceptance` |
| 스타일 검사 | golangci-lint + 라이선스 헤더 | `make test-style` |

Helm v4는 테스트에서 `*_test.go` 파일에 Golden 파일 비교 패턴을 광범위하게 사용한다. `make gen-test-golden`으로 기대값을 재생성할 수 있다.
