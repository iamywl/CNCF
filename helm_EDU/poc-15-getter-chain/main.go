package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// =============================================================================
// Helm Getter 체인 PoC
// =============================================================================
//
// 참조: pkg/getter/getter.go, pkg/getter/httpgetter.go, pkg/getter/ocigetter.go
//
// Helm의 Getter 시스템은 URL 스킴에 따라 적절한 다운로더를 선택한다.
// 이 PoC는 다음을 시뮬레이션한다:
//   1. Getter 인터페이스 — URL로 콘텐츠 다운로드
//   2. Provider — 스킴→Getter 매핑
//   3. HTTP/OCI/파일 스킴 지원
//   4. net/http/httptest로 실제 다운로드 시뮬레이션
// =============================================================================

// --- Getter 인터페이스 ---
// Helm 소스: pkg/getter/getter.go의 Getter 인터페이스
type Getter interface {
	Get(url string, options ...Option) (*bytes.Buffer, error)
}

// --- Option: Getter 설정 옵션 (Functional Options 패턴) ---
// Helm 소스: pkg/getter/getter.go의 Option 타입
type getterOptions struct {
	url                   string
	username              string
	password              string
	userAgent             string
	timeout               time.Duration
	insecureSkipVerifyTLS bool
	acceptHeader          string
}

type Option func(*getterOptions)

func WithURL(url string) Option {
	return func(o *getterOptions) { o.url = url }
}

func WithBasicAuth(user, pass string) Option {
	return func(o *getterOptions) {
		o.username = user
		o.password = pass
	}
}

func WithUserAgent(ua string) Option {
	return func(o *getterOptions) { o.userAgent = ua }
}

func WithTimeout(t time.Duration) Option {
	return func(o *getterOptions) { o.timeout = t }
}

func WithAcceptHeader(h string) Option {
	return func(o *getterOptions) { o.acceptHeader = h }
}

// --- Constructor: Getter 팩토리 함수 ---
// Helm 소스: pkg/getter/getter.go의 Constructor 타입
type Constructor func(options ...Option) (Getter, error)

// --- Provider: 스킴→Getter 매핑 ---
// Helm 소스: pkg/getter/getter.go의 Provider 구조체
type Provider struct {
	Schemes []string
	New     Constructor
}

// Provides는 주어진 스킴을 지원하는지 확인한다.
// Helm 소스: pkg/getter/getter.go의 Provides
func (p Provider) Provides(scheme string) bool {
	for _, s := range p.Schemes {
		if s == scheme {
			return true
		}
	}
	return false
}

// --- Providers: Provider 컬렉션 ---
// Helm 소스: pkg/getter/getter.go의 Providers 타입
type Providers []Provider

// ByScheme은 스킴에 맞는 Getter를 반환한다.
// Helm 소스: pkg/getter/getter.go의 ByScheme
func (p Providers) ByScheme(scheme string) (Getter, error) {
	for _, pp := range p {
		if pp.Provides(scheme) {
			return pp.New()
		}
	}
	return nil, fmt.Errorf("스킴 %q 미지원", scheme)
}

// --- HTTPGetter: HTTP/HTTPS 다운로더 ---
// Helm 소스: pkg/getter/httpgetter.go의 HTTPGetter
type HTTPGetter struct {
	opts getterOptions
}

func NewHTTPGetter(options ...Option) (Getter, error) {
	g := &HTTPGetter{}
	for _, opt := range options {
		opt(&g.opts)
	}
	return g, nil
}

func (g *HTTPGetter) Get(href string, options ...Option) (*bytes.Buffer, error) {
	// 로컬 옵션 복사 (동시성 안전)
	opts := g.opts
	for _, opt := range options {
		opt(&opts)
	}

	req, err := http.NewRequest(http.MethodGet, href, nil)
	if err != nil {
		return nil, err
	}

	// User-Agent 설정
	ua := "Helm/4.0.0"
	if opts.userAgent != "" {
		ua = opts.userAgent
	}
	req.Header.Set("User-Agent", ua)

	// Accept 헤더
	if opts.acceptHeader != "" {
		req.Header.Set("Accept", opts.acceptHeader)
	}

	// Basic Auth
	if opts.username != "" && opts.password != "" {
		req.SetBasicAuth(opts.username, opts.password)
	}

	client := &http.Client{
		Timeout: opts.timeout,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 요청 실패: %s", href, resp.Status)
	}

	buf := bytes.NewBuffer(nil)
	_, err = io.Copy(buf, resp.Body)
	return buf, err
}

// --- OCIGetter: OCI 레지스트리 다운로더 (시뮬레이션) ---
// Helm 소스: pkg/getter/ocigetter.go의 OCIGetter
type OCIGetter struct {
	opts getterOptions
}

func NewOCIGetter(options ...Option) (Getter, error) {
	g := &OCIGetter{}
	for _, opt := range options {
		opt(&g.opts)
	}
	return g, nil
}

func (g *OCIGetter) Get(href string, options ...Option) (*bytes.Buffer, error) {
	// OCI 스킴: oci://registry.example.com/charts/nginx:1.0.0
	fmt.Printf("    [OCI] 다운로드 시뮬레이션: %s\n", href)

	// 실제 구현에서는 ORAS/containerd 클라이언트로 OCI artifact를 가져온다
	content := fmt.Sprintf("# OCI artifact from %s\napiVersion: v2\nname: chart\nversion: 1.0.0\n", href)
	return bytes.NewBufferString(content), nil
}

// --- FileGetter: 로컬 파일 다운로더 ---
type FileGetter struct {
	opts getterOptions
}

func NewFileGetter(options ...Option) (Getter, error) {
	g := &FileGetter{}
	for _, opt := range options {
		opt(&g.opts)
	}
	return g, nil
}

func (g *FileGetter) Get(href string, options ...Option) (*bytes.Buffer, error) {
	path := strings.TrimPrefix(href, "file://")
	fmt.Printf("    [File] 로컬 파일 읽기: %s\n", path)

	// 시뮬레이션: 실제로는 os.ReadFile 사용
	content := fmt.Sprintf("# Local file: %s\nname: local-chart\nversion: 0.1.0\n", path)
	return bytes.NewBufferString(content), nil
}

// --- Getters: 기본 Provider 목록 ---
// Helm 소스: pkg/getter/getter.go의 Getters 함수
func DefaultGetters() Providers {
	return Providers{
		Provider{
			Schemes: []string{"http", "https"},
			New:     NewHTTPGetter,
		},
		Provider{
			Schemes: []string{"oci"},
			New:     NewOCIGetter,
		},
		Provider{
			Schemes: []string{"file"},
			New:     NewFileGetter,
		},
	}
}

// --- 스킴 추출 유틸리티 ---
func extractScheme(url string) string {
	if idx := strings.Index(url, "://"); idx > 0 {
		return url[:idx]
	}
	return ""
}

// --- GetContent: URL에서 콘텐츠 다운로드 (통합 API) ---
func GetContent(providers Providers, url string, opts ...Option) (*bytes.Buffer, error) {
	scheme := extractScheme(url)
	if scheme == "" {
		return nil, fmt.Errorf("URL에 스킴이 없음: %s", url)
	}

	getter, err := providers.ByScheme(scheme)
	if err != nil {
		return nil, err
	}

	return getter.Get(url, opts...)
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              Helm Getter 체인 PoC                            ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("참조: pkg/getter/getter.go, pkg/getter/httpgetter.go,")
	fmt.Println("      pkg/getter/ocigetter.go")
	fmt.Println()

	// =================================================================
	// 1. Getter 인터페이스와 Provider
	// =================================================================
	fmt.Println("1. Getter 인터페이스와 Provider 패턴")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  Getter 인터페이스:
  ┌──────────────────────────────────────────┐
  │  type Getter interface {                  │
  │    Get(url string, opts ...Option)        │
  │        (*bytes.Buffer, error)             │
  │  }                                        │
  └──────────────────────────────────────────┘

  Provider (스킴 → Getter 매핑):
  ┌──────────────────────────────────────────┐
  │  type Provider struct {                   │
  │    Schemes []string      // ["http","https"]│
  │    New     Constructor   // Getter 팩토리  │
  │  }                                        │
  └──────────────────────────────────────────┘

  Providers.ByScheme(scheme) → Getter
  ┌────────┬────────────────────┐
  │ 스킴    │ Getter              │
  ├────────┼────────────────────┤
  │ http   │ HTTPGetter           │
  │ https  │ HTTPGetter           │
  │ oci    │ OCIGetter            │
  │ file   │ FileGetter           │
  │ s3     │ PluginGetter (확장)  │
  │ gs     │ PluginGetter (확장)  │
  └────────┴────────────────────┘
`)

	providers := DefaultGetters()
	fmt.Println("  등록된 Provider:")
	for _, p := range providers {
		fmt.Printf("    스킴: %-15v → %T\n", p.Schemes, func() Getter {
			g, _ := p.New()
			return g
		}())
	}

	// =================================================================
	// 2. HTTP Getter (httptest 서버 사용)
	// =================================================================
	fmt.Println("\n2. HTTP Getter — httptest 서버로 다운로드 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))

	// 테스트 HTTP 서버 생성
	indexYAML := `apiVersion: v1
entries:
  nginx:
    - name: nginx
      version: 15.4.0
      urls:
        - https://charts.example.com/nginx-15.4.0.tgz
  mysql:
    - name: mysql
      version: 9.14.4`

	chartTGZ := "FAKE_CHART_TGZ_CONTENT_FOR_SIMULATION"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("    [서버] %s %s (User-Agent: %s)\n", r.Method, r.URL.Path, r.Header.Get("User-Agent"))

		switch r.URL.Path {
		case "/index.yaml":
			w.Header().Set("Content-Type", "application/x-yaml")
			w.Write([]byte(indexYAML))
		case "/nginx-15.4.0.tgz":
			// Basic Auth 확인
			user, pass, ok := r.BasicAuth()
			if ok {
				fmt.Printf("    [서버] Basic Auth: user=%s\n", user)
				_ = pass
			}
			w.Header().Set("Content-Type", "application/gzip")
			w.Write([]byte(chartTGZ))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	fmt.Printf("\n  테스트 서버: %s\n", ts.URL)

	// index.yaml 다운로드
	fmt.Println("\n  2a. index.yaml 다운로드:")
	buf, err := GetContent(providers, ts.URL+"/index.yaml",
		WithUserAgent("Helm/4.0.0-poc"),
		WithTimeout(10*time.Second),
	)
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	} else {
		fmt.Printf("    다운로드 크기: %d bytes\n", buf.Len())
		fmt.Println("    내용 (처음 100자):")
		content := buf.String()
		if len(content) > 100 {
			content = content[:100] + "..."
		}
		fmt.Printf("    %s\n", content)
	}

	// 차트 다운로드 (Basic Auth 포함)
	fmt.Println("\n  2b. 차트 다운로드 (Basic Auth):")
	buf, err = GetContent(providers, ts.URL+"/nginx-15.4.0.tgz",
		WithBasicAuth("admin", "secret"),
		WithURL(ts.URL),
	)
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	} else {
		fmt.Printf("    다운로드 크기: %d bytes\n", buf.Len())
	}

	// 404 에러
	fmt.Println("\n  2c. 존재하지 않는 차트:")
	_, err = GetContent(providers, ts.URL+"/nonexistent.tgz")
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	}

	// =================================================================
	// 3. OCI Getter
	// =================================================================
	fmt.Println("\n3. OCI Getter — OCI 레지스트리 다운로드")
	fmt.Println(strings.Repeat("-", 60))

	ociURL := "oci://registry.example.com/charts/nginx:15.4.0"
	fmt.Printf("  URL: %s\n", ociURL)

	buf, err = GetContent(providers, ociURL)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  결과:\n    %s\n", strings.ReplaceAll(buf.String(), "\n", "\n    "))
	}

	// =================================================================
	// 4. File Getter
	// =================================================================
	fmt.Println("\n4. File Getter — 로컬 파일 읽기")
	fmt.Println(strings.Repeat("-", 60))

	fileURL := "file:///home/user/charts/local-chart-0.1.0.tgz"
	fmt.Printf("  URL: %s\n", fileURL)

	buf, err = GetContent(providers, fileURL)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  결과:\n    %s\n", strings.ReplaceAll(buf.String(), "\n", "\n    "))
	}

	// =================================================================
	// 5. 미지원 스킴
	// =================================================================
	fmt.Println("\n5. 미지원 스킴 처리")
	fmt.Println(strings.Repeat("-", 60))

	_, err = GetContent(providers, "s3://my-bucket/charts/nginx-15.4.0.tgz")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		fmt.Println("  → s3:// 스킴은 getter 플러그인(helm-s3)을 설치해야 사용 가능")
	}

	// =================================================================
	// 6. Functional Options 패턴
	// =================================================================
	fmt.Println("\n6. Functional Options 패턴")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  Helm의 Getter는 Functional Options 패턴으로 설정한다:")
	fmt.Println(`
  getter.Get(url,
    WithBasicAuth("user", "pass"),    // 인증
    WithTimeout(30 * time.Second),    // 타임아웃
    WithUserAgent("Helm/4.0.0"),      // User-Agent
    WithURL(baseURL),                 // 기준 URL
    WithTLSClientConfig(cert, key, ca), // TLS 설정
    WithInsecureSkipVerifyTLS(true),  // TLS 검증 스킵
    WithPlainHTTP(true),              // HTTP 허용
    WithAcceptHeader("application/json"), // Accept 헤더
  )
`)

	// =================================================================
	// 7. 플러그인 확장 (Plugin Getter)
	// =================================================================
	fmt.Println("7. 플러그인으로 Getter 확장")
	fmt.Println(strings.Repeat("-", 60))

	// S3 플러그인 시뮬레이션
	s3Getter := Provider{
		Schemes: []string{"s3"},
		New: func(options ...Option) (Getter, error) {
			return &S3Getter{}, nil
		},
	}

	extendedProviders := append(providers, s3Getter)

	fmt.Println("  S3 Getter 플러그인 추가 후:")
	for _, p := range extendedProviders {
		fmt.Printf("    스킴: %v\n", p.Schemes)
	}

	buf, err = GetContent(extendedProviders, "s3://my-bucket/charts/nginx-15.4.0.tgz")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  결과:\n    %s\n", strings.ReplaceAll(buf.String(), "\n", "\n    "))
	}

	// =================================================================
	// 8. 아키텍처 다이어그램
	// =================================================================
	fmt.Println("\n8. Getter 체인 아키텍처")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  helm pull https://charts.example.com/nginx-15.4.0.tgz
       │
       v
  ┌──────────────────────┐
  │ URL 스킴 추출: "https"│
  └──────────┬───────────┘
             │
             v
  ┌──────────────────────────────────────────┐
  │ Providers.ByScheme("https")               │
  │                                            │
  │  Provider{Schemes: ["http","https"]}       │
  │  → HTTPGetter 생성                          │
  │                                            │
  │  Provider{Schemes: ["oci"]}                │
  │  → OCIGetter 생성                           │
  │                                            │
  │  Provider{Schemes: ["s3"]}  ← 플러그인     │
  │  → PluginGetter 생성                        │
  └──────────┬─────────────────────────────────┘
             │
             v
  ┌──────────────────────────────────────────┐
  │ HTTPGetter.Get(url, options...)            │
  │                                            │
  │  1. http.Request 생성                      │
  │  2. User-Agent: Helm/4.0.0 설정           │
  │  3. Basic Auth (URL 호스트 매치 검사)       │
  │  4. TLS 설정 (인증서/CA)                   │
  │  5. http.Client.Do(req)                    │
  │  6. 응답 → bytes.Buffer                    │
  └──────────────────────────────────────────┘

  Getter 확장 (Helm v4 플러그인):
  ┌──────────────────────────────────────────┐
  │  plugin.yaml:                             │
  │    type: getter/v1                        │
  │    config:                                │
  │      protocols: [s3, gs, swift]           │
  │                                           │
  │  → getter.All()에서 플러그인 Getter 수집    │
  │  → Providers에 추가                        │
  └──────────────────────────────────────────┘
`)
}

// --- S3Getter: 플러그인 Getter 시뮬레이션 ---
type S3Getter struct{}

func (g *S3Getter) Get(href string, options ...Option) (*bytes.Buffer, error) {
	fmt.Printf("    [S3] 다운로드 시뮬레이션: %s\n", href)
	content := fmt.Sprintf("# S3 artifact from %s\napiVersion: v2\nname: nginx\nversion: 15.4.0\n", href)
	return bytes.NewBufferString(content), nil
}
