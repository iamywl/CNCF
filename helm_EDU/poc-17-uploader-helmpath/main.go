// Package main은 Helm의 Uploader, Helmpath, Version Management의 핵심 개념을
// 시뮬레이션한다.
//
// 시뮬레이션하는 핵심 개념:
// 1. ChartUploader (전략 패턴 기반 프로토콜별 업로드)
// 2. Helmpath lazypath (XDG 기반 경로 해석, 환경 변수 우선순위)
// 3. Version Management (BuildInfo, client-go 버전 매핑)
// 4. Pusher Provider 체인
// 5. CacheIndexFile / CacheChartsFile 경로 생성
//
// 실행: go run main.go
package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ─────────────────────────────────────────────
// 1. Uploader (전략 패턴)
// ─────────────────────────────────────────────

// Pusher는 프로토콜별 업로드를 담당한다.
type Pusher interface {
	Push(ref, dest string, opts ...PushOption) error
}

// PushOption은 함수형 옵션 패턴이다.
type PushOption func(*pushOptions)

type pushOptions struct {
	insecureSkipVerify bool
	username           string
	password           string
}

func WithInsecureSkipVerify(v bool) PushOption {
	return func(o *pushOptions) { o.insecureSkipVerify = v }
}

func WithBasicAuth(user, pass string) PushOption {
	return func(o *pushOptions) { o.username = user; o.password = pass }
}

// OCIPusher는 OCI 레지스트리에 차트를 푸시한다.
type OCIPusher struct{}

func (p *OCIPusher) Push(ref, dest string, opts ...PushOption) error {
	o := &pushOptions{}
	for _, opt := range opts {
		opt(o)
	}
	fmt.Printf("  [OCI] 푸시: %s → %s\n", ref, dest)
	if o.username != "" {
		fmt.Printf("  [OCI] 인증: %s/***\n", o.username)
	}
	if o.insecureSkipVerify {
		fmt.Printf("  [OCI] TLS 검증 건너뛰기\n")
	}
	return nil
}

// PusherProviders는 프로토콜별 Pusher를 관리한다.
type PusherProviders struct {
	providers map[string]Pusher
}

func NewPusherProviders() *PusherProviders {
	return &PusherProviders{
		providers: map[string]Pusher{
			"oci": &OCIPusher{},
		},
	}
}

func (pp *PusherProviders) ByScheme(scheme string) (Pusher, error) {
	p, ok := pp.providers[scheme]
	if !ok {
		return nil, fmt.Errorf("unsupported scheme: %q", scheme)
	}
	return p, nil
}

// ChartUploader는 차트 업로드를 관리한다.
type ChartUploader struct {
	Pushers *PusherProviders
	Options []PushOption
}

func (c *ChartUploader) UploadTo(ref, remote string) error {
	u, err := url.Parse(remote)
	if err != nil {
		return fmt.Errorf("invalid chart URL format: %s", remote)
	}

	if u.Scheme == "" {
		return fmt.Errorf("scheme prefix missing from remote (e.g. \"oci://\")")
	}

	p, err := c.Pushers.ByScheme(u.Scheme)
	if err != nil {
		return err
	}

	return p.Push(ref, u.String(), c.Options...)
}

// ─────────────────────────────────────────────
// 2. Helmpath (XDG 기반 경로 시스템)
// ─────────────────────────────────────────────

const (
	CacheHomeEnvVar  = "HELM_CACHE_HOME"
	ConfigHomeEnvVar = "HELM_CONFIG_HOME"
	DataHomeEnvVar   = "HELM_DATA_HOME"
	XDGCacheHome     = "XDG_CACHE_HOME"
	XDGConfigHome    = "XDG_CONFIG_HOME"
	XDGDataHome      = "XDG_DATA_HOME"
)

// lazypath는 XDG 기반 디렉토리 사양의 지연 로딩 경로이다.
type lazypath string

func (l lazypath) path(helmEnvVar, xdgEnvVar string, defaultFn func() string, elem ...string) string {
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

func (l lazypath) configPath(elem ...string) string {
	return l.path(ConfigHomeEnvVar, XDGConfigHome, configHome, filepath.Join(elem...))
}

func (l lazypath) cachePath(elem ...string) string {
	return l.path(CacheHomeEnvVar, XDGCacheHome, cacheHome, filepath.Join(elem...))
}

func (l lazypath) dataPath(elem ...string) string {
	return l.path(DataHomeEnvVar, XDGDataHome, dataHome, filepath.Join(elem...))
}

// OS별 기본 경로
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/tmp"
}

func configHome() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir(), "Library", "Preferences")
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return appdata
		}
		return filepath.Join(homeDir(), "AppData", "Roaming")
	default: // linux
		return filepath.Join(homeDir(), ".config")
	}
}

func cacheHome() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir(), "Library", "Caches")
	case "windows":
		if appdata := os.Getenv("LOCALAPPDATA"); appdata != "" {
			return appdata
		}
		return filepath.Join(homeDir(), "AppData", "Local")
	default:
		return filepath.Join(homeDir(), ".cache")
	}
}

func dataHome() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir(), "Library")
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return appdata
		}
		return filepath.Join(homeDir(), "AppData", "Roaming")
	default:
		return filepath.Join(homeDir(), ".local", "share")
	}
}

// 공개 API
var lp = lazypath("helm")

func ConfigPath(elem ...string) string { return lp.configPath(elem...) }
func CachePath(elem ...string) string  { return lp.cachePath(elem...) }
func DataPath(elem ...string) string   { return lp.dataPath(elem...) }

func CacheIndexFile(name string) string {
	if name != "" {
		name += "-"
	}
	return name + "index.yaml"
}

func CacheChartsFile(name string) string {
	if name != "" {
		name += "-"
	}
	return name + "charts.txt"
}

// ─────────────────────────────────────────────
// 3. Version Management
// ─────────────────────────────────────────────

var (
	version      = "v4.1"
	metadata     = ""
	gitCommit    = "abc123def456"
	gitTreeState = "clean"
)

type BuildInfo struct {
	Version           string
	GitCommit         string
	GitTreeState      string
	GoVersion         string
	KubeClientVersion string
}

func GetVersion() string {
	if metadata == "" {
		return version
	}
	return version + "+" + metadata
}

func GetUserAgent() string {
	return "Helm/" + strings.TrimPrefix(GetVersion(), "v")
}

func Get() BuildInfo {
	// client-go 버전 매핑: v0.30.x → v1.30
	// 실제 소스에서는 debug.ReadBuildInfo()로 client-go 모듈 버전을 조회한다.
	// 매핑 규칙: client-go v0.X.Y → Kubernetes v1.X
	major := 0 + 1 // client-go Major(0) + 1
	minor := 30     // client-go Minor

	return BuildInfo{
		Version:           GetVersion(),
		GitCommit:         gitCommit,
		GitTreeState:      gitTreeState,
		GoVersion:         runtime.Version(),
		KubeClientVersion: fmt.Sprintf("v%d.%d", major, minor),
	}
}

// ─────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║  Helm Uploader, Helmpath, Version 시뮬레이션     ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. ChartUploader 데모 ===
	fmt.Println("━━━ 1. ChartUploader ━━━")
	uploader := &ChartUploader{
		Pushers: NewPusherProviders(),
		Options: []PushOption{
			WithBasicAuth("admin", "secret"),
		},
	}

	// 정상 업로드
	err := uploader.UploadTo("mychart-1.0.0.tgz", "oci://registry.example.com/charts")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	}

	// 스킴 누락 테스트
	err = uploader.UploadTo("mychart.tgz", "registry.example.com/charts")
	if err != nil {
		fmt.Printf("  스킴 누락 오류: %v\n", err)
	}

	// 미지원 프로토콜 테스트
	err = uploader.UploadTo("mychart.tgz", "ftp://registry.example.com/charts")
	if err != nil {
		fmt.Printf("  미지원 프로토콜 오류: %v\n", err)
	}
	fmt.Println()

	// === 2. Helmpath 경로 해석 데모 ===
	fmt.Println("━━━ 2. Helmpath 경로 시스템 ━━━")
	fmt.Printf("  OS: %s\n", runtime.GOOS)
	fmt.Printf("  ConfigPath():                %s\n", ConfigPath())
	fmt.Printf("  CachePath():                 %s\n", CachePath())
	fmt.Printf("  DataPath():                  %s\n", DataPath())
	fmt.Printf("  ConfigPath(\"repositories.yaml\"): %s\n", ConfigPath("repositories.yaml"))
	fmt.Printf("  CachePath(\"repository\"):          %s\n", CachePath("repository"))
	fmt.Printf("  DataPath(\"plugins\", \"helm-diff\"): %s\n", DataPath("plugins", "helm-diff"))
	fmt.Println()

	// 환경 변수 우선순위 테스트
	fmt.Println("  [환경 변수 우선순위 테스트]")

	// 저장 및 복원
	origCache := os.Getenv(CacheHomeEnvVar)
	origXDG := os.Getenv(XDGCacheHome)

	// 테스트 1: Helm 환경 변수 설정
	os.Setenv(CacheHomeEnvVar, "/custom/helm/cache")
	fmt.Printf("  HELM_CACHE_HOME=/custom/helm/cache → %s\n", CachePath())

	// 테스트 2: XDG 환경 변수 (Helm 변수가 우선)
	os.Setenv(XDGCacheHome, "/xdg/cache")
	fmt.Printf("  +XDG_CACHE_HOME=/xdg/cache         → %s (Helm 우선)\n", CachePath())

	// 테스트 3: Helm 변수 제거 → XDG 사용
	os.Unsetenv(CacheHomeEnvVar)
	fmt.Printf("  HELM 변수 제거, XDG만             → %s\n", CachePath())

	// 테스트 4: 둘 다 제거 → 기본값
	os.Unsetenv(XDGCacheHome)
	fmt.Printf("  둘 다 제거 → 기본값                → %s\n", CachePath())

	// 복원
	if origCache != "" {
		os.Setenv(CacheHomeEnvVar, origCache)
	}
	if origXDG != "" {
		os.Setenv(XDGCacheHome, origXDG)
	}
	fmt.Println()

	// === 3. 캐시 파일 경로 생성 ===
	fmt.Println("━━━ 3. 캐시 파일 경로 ━━━")
	repos := []string{"stable", "bitnami", "grafana", ""}
	for _, repo := range repos {
		idx := CacheIndexFile(repo)
		charts := CacheChartsFile(repo)
		name := repo
		if name == "" {
			name = "(빈 이름)"
		}
		fmt.Printf("  %-12s → 인덱스: %-25s 차트: %s\n", name, idx, charts)
	}
	fmt.Println()

	// === 4. Version Management 데모 ===
	fmt.Println("━━━ 4. Version Management ━━━")
	bi := Get()
	fmt.Printf("  Version:           %s\n", bi.Version)
	fmt.Printf("  GitCommit:         %s\n", bi.GitCommit)
	fmt.Printf("  GitTreeState:      %s\n", bi.GitTreeState)
	fmt.Printf("  GoVersion:         %s\n", bi.GoVersion)
	fmt.Printf("  KubeClientVersion: %s\n", bi.KubeClientVersion)
	fmt.Printf("  UserAgent:         %s\n", GetUserAgent())

	// client-go 버전 매핑 테이블
	fmt.Println("\n  [client-go → Kubernetes 버전 매핑]")
	fmt.Printf("  %-15s → %-15s\n", "client-go", "Kubernetes")
	fmt.Println("  " + strings.Repeat("─", 35))
	clientGoVersions := []struct {
		version string
		major   int
		minor   int
	}{
		{"v0.28.0", 1, 28},
		{"v0.29.0", 1, 29},
		{"v0.30.0", 1, 30},
		{"v0.31.0", 1, 31},
	}
	for _, cv := range clientGoVersions {
		fmt.Printf("  %-15s → v%d.%d\n", cv.version, cv.major, cv.minor)
	}

	// 메타데이터 포함 버전
	fmt.Println("\n  [메타데이터 포함 버전]")
	metadata = "build.123"
	fmt.Printf("  metadata=\"%s\" → %s\n", metadata, GetVersion())
	fmt.Printf("  UserAgent: %s\n", GetUserAgent())
	metadata = "" // 복원

	fmt.Println()

	// === 5. OS별 기본 경로 비교 ===
	fmt.Println("━━━ 5. OS별 기본 경로 비교 ━━━")
	fmt.Printf("  %-10s %-10s %-30s\n", "유형", "현재 OS", "경로")
	fmt.Println("  " + strings.Repeat("─", 55))
	fmt.Printf("  %-10s %-10s %-30s\n", "설정", runtime.GOOS, configHome())
	fmt.Printf("  %-10s %-10s %-30s\n", "캐시", runtime.GOOS, cacheHome())
	fmt.Printf("  %-10s %-10s %-30s\n", "데이터", runtime.GOOS, dataHome())

	fmt.Println()
	fmt.Println("시뮬레이션 완료.")
}
