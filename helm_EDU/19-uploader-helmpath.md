# 19. Uploader, Helmpath, Version Management — 패키징 & 경로 Deep Dive

## 목차

1. [개요](#1-개요)
2. [Chart Uploader 아키텍처](#2-chart-uploader-아키텍처)
3. [Uploader 실행 흐름](#3-uploader-실행-흐름)
4. [Pusher 프로바이더 체인](#4-pusher-프로바이더-체인)
5. [Helmpath 경로 시스템](#5-helmpath-경로-시스템)
6. [Lazypath 지연 로딩 패턴](#6-lazypath-지연-로딩-패턴)
7. [XDG Base Directory 호환](#7-xdg-base-directory-호환)
8. [OS별 기본 경로](#8-os별-기본-경로)
9. [Version Management 시스템](#9-version-management-시스템)
10. [BuildInfo 구조](#10-buildinfo-구조)
11. [Client-Go 버전 감지](#11-client-go-버전-감지)
12. [통합 설계 분석](#12-통합-설계-분석)

---

## 1. 개요

이 문서에서 다루는 세 가지 서브시스템은 Helm의 **인프라 계층**을 구성한다:

| 서브시스템 | 패키지 | 역할 |
|-----------|--------|------|
| **Uploader** | `pkg/uploader/` | 차트 패키지를 원격 레지스트리에 업로드 |
| **Helmpath** | `pkg/helmpath/` | Helm 설정/캐시/데이터 경로 관리 |
| **Version** | `internal/version/` | Helm 빌드 버전 및 K8s 호환성 정보 |

### 왜 이 컴포넌트들이 중요한가

```
┌─────────────────────────────────────────────────────┐
│                   Helm CLI                           │
│                                                     │
│  helm push ──→ Uploader ──→ OCI Registry            │
│                                                     │
│  helm install ──→ Helmpath ──→ 캐시/설정 디렉토리     │
│                                                     │
│  helm version ──→ Version ──→ 빌드/K8s 호환 정보     │
└─────────────────────────────────────────────────────┘
```

---

## 2. Chart Uploader 아키텍처

### 소스 코드 구조

```
pkg/uploader/
├── doc.go              ← 패키지 문서
└── chart_uploader.go   ← ChartUploader 구조체, UploadTo 메서드
```

### ChartUploader 구조체

`pkg/uploader/chart_uploader.go` 라인 28-37:

```go
type ChartUploader struct {
    Out            io.Writer          // 경고/정보 메시지 출력 위치
    Pushers        pusher.Providers   // 프로토콜별 Pusher 컬렉션
    Options        []pusher.Option    // Pusher에 전달할 옵션
    RegistryClient *registry.Client   // OCI 레지스트리 클라이언트
}
```

**왜 이 설계인가?**

ChartUploader는 **전략 패턴(Strategy Pattern)**을 사용한다:
- `Pushers`가 프로토콜별 업로드 전략을 제공
- `Options`로 인증, TLS 등의 설정을 전달
- `RegistryClient`로 OCI 특화 기능 사용

이 설계로 새로운 프로토콜(예: S3, Git)을 추가할 때 기존 코드를 변경하지 않아도 된다.

---

## 3. Uploader 실행 흐름

### UploadTo 메서드

`pkg/uploader/chart_uploader.go` 라인 40-56:

```go
func (c *ChartUploader) UploadTo(ref, remote string) error {
    // 1. URL 파싱
    u, err := url.Parse(remote)
    if err != nil {
        return fmt.Errorf("invalid chart URL format: %s", remote)
    }

    // 2. 스킴 검증
    if u.Scheme == "" {
        return fmt.Errorf("scheme prefix missing from remote (e.g. \"%s://\")",
            registry.OCIScheme)
    }

    // 3. 스킴 기반 Pusher 선택
    p, err := c.Pushers.ByScheme(u.Scheme)
    if err != nil {
        return err
    }

    // 4. 업로드 실행
    return p.Push(ref, u.String(), c.Options...)
}
```

### 실행 시퀀스

```
사용자: helm push mychart.tgz oci://registry.example.com/charts
    │
    ▼
UploadTo("mychart.tgz", "oci://registry.example.com/charts")
    │
    ├─ 1. url.Parse("oci://registry.example.com/charts")
    │     → Scheme: "oci", Host: "registry.example.com", Path: "/charts"
    │
    ├─ 2. Scheme 확인: "oci" != "" → 통과
    │
    ├─ 3. Pushers.ByScheme("oci")
    │     → OCIPusher 반환
    │
    └─ 4. OCIPusher.Push("mychart.tgz", "oci://...", opts...)
          → 차트를 OCI 레지스트리에 업로드
```

### 에러 처리 설계

| 에러 | 원인 | 사용자 메시지 |
|------|------|-------------|
| URL 파싱 실패 | 잘못된 URL 형식 | `invalid chart URL format: ...` |
| 스킴 누락 | `oci://` 없이 입력 | `scheme prefix missing from remote (e.g. "oci://")` |
| Pusher 없음 | 미지원 프로토콜 | Pusher.ByScheme 에러 반환 |
| Push 실패 | 네트워크/인증 오류 | Pusher 구현체의 에러 |

**왜 스킴 검증을 명시적으로 하는가?** 사용자가 `registry.example.com/charts`처럼 스킴 없이 입력하는 것은 흔한 실수이다. 이를 명시적으로 검사하여 의미 있는 에러 메시지(예: `oci://`를 추가하라는 안내)를 제공한다.

---

## 4. Pusher 프로바이더 체인

### Providers 인터페이스

```
pkg/pusher/
├── providers.go    ← Providers 인터페이스 (ByScheme 메서드)
└── oci.go          ← OCIPusher (OCI 레지스트리용)
```

```
pusher.Providers
    │
    └─ ByScheme(scheme string) → Pusher
         │
         ├─ "oci" → OCIPusher
         └─ 기타  → 에러
```

### pusher.Option 패턴

```go
type Option func(*options)

// 사용 예:
pusher.WithTLSClientConfig(certFile, keyFile, caFile)
pusher.WithInsecureSkipTLSVerify(true)
pusher.WithRegistryClient(registryClient)
```

**함수형 옵션 패턴(Functional Options)**을 사용하여:
1. 선택적 파라미터를 깔끔하게 전달
2. 새 옵션 추가 시 기존 API 비호환 방지
3. 기본값을 자연스럽게 처리

---

## 5. Helmpath 경로 시스템

### 소스 코드 구조

```
pkg/helmpath/
├── home.go                ← 공개 API (ConfigPath, CachePath, DataPath)
├── lazypath.go            ← lazypath 타입, 경로 해석 로직
├── lazypath_darwin.go     ← macOS 기본 경로
├── lazypath_unix.go       ← Linux/Unix 기본 경로
├── lazypath_windows.go    ← Windows 기본 경로
└── xdg/                   ← XDG 환경 변수 상수
```

### 공개 API

`pkg/helmpath/home.go`:

```go
const lp = lazypath("helm")

func ConfigPath(elem ...string) string { return lp.configPath(elem...) }
func CachePath(elem ...string) string  { return lp.cachePath(elem...) }
func DataPath(elem ...string) string   { return lp.dataPath(elem...) }

func CacheIndexFile(name string) string {
    if name != "" { name += "-" }
    return name + "index.yaml"
}

func CacheChartsFile(name string) string {
    if name != "" { name += "-" }
    return name + "charts.txt"
}
```

### 경로 유형

| 함수 | 용도 | 예시 내용 |
|------|------|----------|
| `ConfigPath()` | Helm 설정 | `repositories.yaml`, 플러그인 설정 |
| `CachePath()` | 캐시 데이터 | 인덱스 파일, 다운로드된 차트 |
| `DataPath()` | 영구 데이터 | 플러그인 바이너리, 스태시 |
| `CacheIndexFile("stable")` | 리포지토리 인덱스 | `stable-index.yaml` |
| `CacheChartsFile("stable")` | 차트 목록 | `stable-charts.txt` |

---

## 6. Lazypath 지연 로딩 패턴

### lazypath 타입

`pkg/helmpath/lazypath.go`:

```go
type lazypath string  // 값: "helm"

func (l lazypath) path(helmEnvVar, xdgEnvVar string, defaultFn func() string,
    elem ...string) string {

    // 우선순위 1: Helm 전용 환경 변수
    base := os.Getenv(helmEnvVar)
    if base != "" {
        return filepath.Join(base, filepath.Join(elem...))
    }

    // 우선순위 2: XDG 환경 변수
    base = os.Getenv(xdgEnvVar)
    if base == "" {
        // 우선순위 3: OS별 기본 경로
        base = defaultFn()
    }

    return filepath.Join(base, string(l), filepath.Join(elem...))
}
```

### 환경 변수 우선순위

```
HELM_CACHE_HOME 설정됨?
    ├─ Yes → $HELM_CACHE_HOME/elem...
    └─ No
        │
        XDG_CACHE_HOME 설정됨?
            ├─ Yes → $XDG_CACHE_HOME/helm/elem...
            └─ No
                │
                OS별 기본 경로
                    ├─ macOS  → ~/Library/Caches/helm/elem...
                    ├─ Linux  → ~/.cache/helm/elem...
                    └─ Windows → %LOCALAPPDATA%/helm/elem...
```

### 환경 변수 상수

```go
const (
    CacheHomeEnvVar  = "HELM_CACHE_HOME"
    ConfigHomeEnvVar = "HELM_CONFIG_HOME"
    DataHomeEnvVar   = "HELM_DATA_HOME"
)
```

**왜 "lazypath"인가?** 경로는 `os.Getenv()`를 **호출 시점에** 평가한다(초기화 시가 아님). 따라서 프로그램 실행 중에 환경 변수가 변경되면 새 값이 반영된다. 이것이 "lazy"(지연)의 의미이다.

---

## 7. XDG Base Directory 호환

### XDG 사양

Helm은 [XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html)을 따른다:

| XDG 변수 | Helm 변수 | 용도 | Linux 기본값 |
|----------|-----------|------|-------------|
| `XDG_CACHE_HOME` | `HELM_CACHE_HOME` | 캐시 | `~/.cache` |
| `XDG_CONFIG_HOME` | `HELM_CONFIG_HOME` | 설정 | `~/.config` |
| `XDG_DATA_HOME` | `HELM_DATA_HOME` | 데이터 | `~/.local/share` |

### 경로 해석 예시

```
[Linux, 환경변수 없음]
ConfigPath("repositories.yaml")
  → ~/.config/helm/repositories.yaml

[Linux, HELM_CONFIG_HOME=/opt/helm]
ConfigPath("repositories.yaml")
  → /opt/helm/repositories.yaml

[macOS, 환경변수 없음]
ConfigPath("repositories.yaml")
  → ~/Library/Preferences/helm/repositories.yaml

[Windows, 환경변수 없음]
ConfigPath("repositories.yaml")
  → %APPDATA%/helm/repositories.yaml
```

### 중첩 경로

```go
// elem... 인자로 하위 경로 구성
CachePath("repository", "stable-index.yaml")
// → ~/.cache/helm/repository/stable-index.yaml

DataPath("plugins", "helm-diff", "bin")
// → ~/.local/share/helm/plugins/helm-diff/bin
```

---

## 8. OS별 기본 경로

### macOS (`lazypath_darwin.go`)

```go
func dataHome() string {
    return filepath.Join(homedir.HomeDir(), "Library")
}

func configHome() string {
    return filepath.Join(homedir.HomeDir(), "Library", "Preferences")
}

func cacheHome() string {
    return filepath.Join(homedir.HomeDir(), "Library", "Caches")
}
```

### Linux/Unix (`lazypath_unix.go`)

```go
func dataHome() string {
    return filepath.Join(homedir.HomeDir(), ".local", "share")
}

func configHome() string {
    return filepath.Join(homedir.HomeDir(), ".config")
}

func cacheHome() string {
    return filepath.Join(homedir.HomeDir(), ".cache")
}
```

### 경로 비교 테이블

| 유형 | macOS | Linux | Windows |
|------|-------|-------|---------|
| 설정 | `~/Library/Preferences/helm/` | `~/.config/helm/` | `%APPDATA%/helm/` |
| 캐시 | `~/Library/Caches/helm/` | `~/.cache/helm/` | `%TEMP%/helm/` |
| 데이터 | `~/Library/helm/` | `~/.local/share/helm/` | `%APPDATA%/helm/` |

**왜 OS별로 다른가?** 각 OS의 관례를 따르기 위함이다. macOS는 `~/Library/` 아래에 앱 데이터를 저장하고, Linux는 XDG 표준을 따르고, Windows는 `%APPDATA%`를 사용한다.

---

## 9. Version Management 시스템

### 소스 코드 구조

```
internal/version/
├── version.go    ← 버전 변수, BuildInfo, GetVersion, GetUserAgent
└── clientgo.go   ← K8s client-go 모듈 버전 감지
```

### 버전 변수

`internal/version/version.go` 라인 30-45:

```go
var (
    version      = "v4.1"      // 현재 버전 (Major.Minor)
    metadata     = ""           // 빌드 메타데이터 (빌드 시 주입)
    gitCommit    = ""           // Git SHA1 (빌드 시 주입)
    gitTreeState = ""           // Git 트리 상태 (빌드 시 주입)
)
```

**빌드 시 값 주입**: `go build -ldflags "-X internal/version.gitCommit=abc123"`로 빌드 시점에 값을 설정한다.

### GetVersion

```go
func GetVersion() string {
    if metadata == "" {
        return version  // "v4.1"
    }
    return version + "+" + metadata  // "v4.1+build.123"
}
```

### GetUserAgent

```go
func GetUserAgent() string {
    return "Helm/" + strings.TrimPrefix(GetVersion(), "v")
    // "Helm/4.1" 또는 "Helm/4.1+build.123"
}
```

HTTP 요청의 `User-Agent` 헤더에 사용된다. 서버 측에서 Helm 클라이언트 버전을 추적할 수 있다.

---

## 10. BuildInfo 구조

### 구조체 정의

```go
type BuildInfo struct {
    Version           string `json:"version,omitempty"`
    GitCommit         string `json:"git_commit,omitempty"`
    GitTreeState      string `json:"git_tree_state,omitempty"`
    GoVersion         string `json:"go_version,omitempty"`
    KubeClientVersion string `json:"kube_client_version"`
}
```

### Get 함수

`internal/version/version.go` 라인 79-120:

```go
func Get() BuildInfo {
    makeKubeClientVersionString := func() string {
        if testing.Testing() {
            return kubeClientGoVersionTesting  // "v1.20"
        }

        vstr, err := K8sIOClientGoModVersion()
        if err != nil {
            slog.Error("failed to retrieve k8s.io/client-go version", ...)
            return ""
        }

        v, _ := semver.NewVersion(vstr)
        // client-go v0.30.x → Kubernetes v1.30
        kubeClientVersionMajor := v.Major() + 1
        kubeClientVersionMinor := v.Minor()
        return fmt.Sprintf("v%d.%d", kubeClientVersionMajor, kubeClientVersionMinor)
    }

    return BuildInfo{
        Version:           GetVersion(),
        GitCommit:         gitCommit,
        GitTreeState:      gitTreeState,
        GoVersion:         runtime.Version(),
        KubeClientVersion: makeKubeClientVersionString(),
    }
}
```

### 출력 예시

```json
{
  "version": "v4.1",
  "git_commit": "abc123def456",
  "git_tree_state": "clean",
  "go_version": "go1.22.0",
  "kube_client_version": "v1.30"
}
```

---

## 11. Client-Go 버전 감지

### K8sIOClientGoModVersion

`internal/version/clientgo.go`:

```go
func K8sIOClientGoModVersion() (string, error) {
    info, ok := debug.ReadBuildInfo()
    if !ok {
        return "", errors.New("failed to read build info")
    }

    idx := slices.IndexFunc(info.Deps, func(m *debug.Module) bool {
        return m.Path == "k8s.io/client-go"
    })

    if idx == -1 {
        return "", errors.New("k8s.io/client-go not found in build info")
    }

    return info.Deps[idx].Version, nil
}
```

### 버전 매핑 논리

```
client-go 모듈 버전:  v0.30.2
                        │
                   Major + 1 = 1
                   Minor = 30
                        │
                        ▼
Kubernetes 버전:    v1.30
```

**왜 Major에 1을 더하는가?** Kubernetes의 `client-go` 모듈은 `v0.X` 버전 체계를 사용한다. `v0.30`은 Kubernetes `v1.30`에 대응한다. 따라서 `Major(0) + 1 = 1`로 변환한다.

### 테스트 환경 처리

```go
if testing.Testing() {
    return kubeClientGoVersionTesting  // "v1.20" 고정
}
```

테스트 빌드는 `debug.ReadBuildInfo()`가 정확한 정보를 포함하지 않으므로, 안정적인 고정값을 반환한다.

---

## 12. 통합 설계 분석

### Helm CLI에서의 사용 흐름

```
helm push mychart.tgz oci://registry.example.com/charts
    │
    ├─ Helmpath: 설정 파일 경로 → 인증 정보 로드
    │   ConfigPath("registry", "config.json")
    │
    ├─ Version: User-Agent 헤더 설정
    │   GetUserAgent() → "Helm/4.1"
    │
    └─ Uploader: 차트 업로드 실행
        UploadTo(ref, remote)
```

### 설정 파일 위치 매핑

```
~/.config/helm/                         # ConfigPath()
├── repositories.yaml                   # 리포지토리 목록
├── registry/
│   └── config.json                     # OCI 레지스트리 인증
└── plugins/                            # 플러그인 설정

~/.cache/helm/                          # CachePath()
├── repository/
│   ├── stable-index.yaml               # CacheIndexFile("stable")
│   └── stable-charts.txt              # CacheChartsFile("stable")
└── plugins/                            # 플러그인 캐시

~/.local/share/helm/                    # DataPath()
├── plugins/                            # 플러그인 바이너리
└── starters/                           # 스타터 차트
```

### 환경 변수 정리

| 변수 | 용도 | 우선순위 |
|------|------|---------|
| `HELM_CACHE_HOME` | 캐시 경로 재정의 | 최우선 |
| `HELM_CONFIG_HOME` | 설정 경로 재정의 | 최우선 |
| `HELM_DATA_HOME` | 데이터 경로 재정의 | 최우선 |
| `XDG_CACHE_HOME` | XDG 캐시 경로 | Helm 변수 없을 때 |
| `XDG_CONFIG_HOME` | XDG 설정 경로 | Helm 변수 없을 때 |
| `XDG_DATA_HOME` | XDG 데이터 경로 | Helm 변수 없을 때 |

### 확장성

| 시나리오 | 관련 컴포넌트 | 설정 |
|----------|-------------|------|
| 새 레지스트리 프로토콜 추가 | Uploader | Pusher.Provider에 새 Pusher 등록 |
| CI/CD 환경 | Helmpath | `HELM_*_HOME` 환경 변수 설정 |
| 클러스터 호환성 확인 | Version | `KubeClientVersion` 확인 |
| 멀티 환경 관리 | Helmpath | 환경별 `HELM_CONFIG_HOME` 설정 |

---

## 부록: 주요 소스 파일 참조

| 파일 | 설명 |
|------|------|
| `pkg/uploader/chart_uploader.go` | ChartUploader 구조체, UploadTo (57줄) |
| `pkg/uploader/doc.go` | 패키지 문서 |
| `pkg/helmpath/home.go` | 공개 API (ConfigPath, CachePath, DataPath) (45줄) |
| `pkg/helmpath/lazypath.go` | lazypath 타입, 경로 해석 로직 (73줄) |
| `pkg/helmpath/lazypath_darwin.go` | macOS 기본 경로 (35줄) |
| `pkg/helmpath/lazypath_unix.go` | Linux/Unix 기본 경로 |
| `pkg/helmpath/lazypath_windows.go` | Windows 기본 경로 |
| `internal/version/version.go` | 버전 변수, BuildInfo (121줄) |
| `internal/version/clientgo.go` | client-go 버전 감지 (45줄) |
