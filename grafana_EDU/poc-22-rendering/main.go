package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Grafana 렌더링(Rendering) 시스템 PoC
//
// 이 PoC는 Grafana의 서버사이드 렌더링 핵심 개념을 시뮬레이션한다:
//   1. RenderingService의 렌더링 요청 처리
//   2. 동시성 제어 (ConcurrentLimit + atomic counter)
//   3. renderKey 인증 (per-request 및 session 기반)
//   4. Capability 시스템 (semver 기반 기능 확인)
//   5. 에러 이미지 반환 패턴
//   6. HTTP 모드 URL 생성
//   7. 렌더링 메트릭 수집
//
// 실제 소스 참조:
//   - pkg/services/rendering/interface.go      (Service 인터페이스, 타입 정의)
//   - pkg/services/rendering/rendering.go      (RenderingService 구현)
//   - pkg/services/rendering/auth.go           (renderKey 인증)
//   - pkg/services/rendering/http_mode.go      (HTTP 모드)
//   - pkg/services/rendering/capabilities.go   (Capability 시스템)
// =============================================================================

// --- 렌더링 타입 (pkg/services/rendering/interface.go 참조) ---

type RenderType string

const (
	RenderCSV RenderType = "csv"
	RenderPNG RenderType = "png"
	RenderPDF RenderType = "pdf"
)

// --- 에러 정의 (pkg/services/rendering/interface.go 참조) ---

type RenderError struct {
	message string
}

func (e *RenderError) Error() string { return e.message }

var (
	ErrTimeout                = &RenderError{"timeout error - you can set timeout in seconds with &timeout url parameter"}
	ErrConcurrentLimitReached = &RenderError{"rendering concurrent limit reached"}
	ErrRenderUnavailable      = &RenderError{"rendering plugin not available"}
)

// --- 렌더링 옵션 (pkg/services/rendering/interface.go 참조) ---

type AuthOpts struct {
	OrgID   int64
	UserID  int64
	OrgRole string
}

type RenderOpts struct {
	Path            string
	Width           int
	Height          int
	DeviceScale     float64
	Theme           string
	Timeout         time.Duration
	ConcurrentLimit int
	AuthOpts        AuthOpts
}

type RenderResult struct {
	FilePath string
}

// --- RenderUser (pkg/services/rendering/auth.go 참조) ---

type RenderUser struct {
	OrgID   int64  `json:"org_id"`
	UserID  int64  `json:"user_id"`
	OrgRole string `json:"org_role"`
}

// --- renderKey 인증 (pkg/services/rendering/auth.go 참조) ---

// renderKeyProvider는 렌더링 인증 키를 관리하는 인터페이스이다.
type renderKeyProvider interface {
	get(opts AuthOpts) (string, error)
	afterRequest(renderKey string)
}

// perRequestRenderKeyProvider는 요청마다 새로운 renderKey를 생성하고 완료 후 삭제한다.
type perRequestRenderKeyProvider struct {
	mu    sync.Mutex
	cache map[string]*RenderUser
}

func newPerRequestRenderKeyProvider() *perRequestRenderKeyProvider {
	return &perRequestRenderKeyProvider{
		cache: make(map[string]*RenderUser),
	}
}

func (p *perRequestRenderKeyProvider) get(opts AuthOpts) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := generateRandomKey()
	p.cache[key] = &RenderUser{
		OrgID:   opts.OrgID,
		UserID:  opts.UserID,
		OrgRole: opts.OrgRole,
	}
	return key, nil
}

func (p *perRequestRenderKeyProvider) afterRequest(renderKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cache, renderKey)
}

func (p *perRequestRenderKeyProvider) lookup(key string) (*RenderUser, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u, ok := p.cache[key]
	return u, ok
}

// sessionRenderKeyProvider는 세션 동안 동일한 renderKey를 재사용한다.
type sessionRenderKeyProvider struct {
	mu        sync.Mutex
	cache     map[string]*RenderUser
	renderKey string
	authOpts  AuthOpts
	expiry    time.Duration
}

func newSessionRenderKeyProvider(cache map[string]*RenderUser, opts AuthOpts, expiry time.Duration) *sessionRenderKeyProvider {
	key := generateRandomKey()
	cache[key] = &RenderUser{
		OrgID:   opts.OrgID,
		UserID:  opts.UserID,
		OrgRole: opts.OrgRole,
	}
	return &sessionRenderKeyProvider{
		cache:     cache,
		renderKey: key,
		authOpts:  opts,
		expiry:    expiry,
	}
}

func (s *sessionRenderKeyProvider) get(opts AuthOpts) (string, error) {
	// 세션에서는 동일한 renderKey를 재사용
	return s.renderKey, nil
}

func (s *sessionRenderKeyProvider) afterRequest(renderKey string) {
	// 세션에서는 요청 후에도 키를 삭제하지 않음
}

func (s *sessionRenderKeyProvider) dispose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, s.renderKey)
}

// --- Capability 시스템 (pkg/services/rendering/capabilities.go 참조) ---

type CapabilityName string

const (
	ScalingDownImages CapabilityName = "ScalingDownImages"
	FullHeightImages  CapabilityName = "FullHeightImages"
	PDFRendering      CapabilityName = "PdfRendering"
)

type Capability struct {
	Name            CapabilityName
	MinMajor        int
	MinMinor        int
	MinPatch        int
}

// --- 메트릭 (pkg/services/rendering/rendering.go 참조) ---

type RenderMetrics struct {
	mu            sync.Mutex
	successCount  map[RenderType]int
	failureCount  map[RenderType]int
	timeoutCount  map[RenderType]int
	totalDuration map[RenderType]time.Duration
}

func NewRenderMetrics() *RenderMetrics {
	return &RenderMetrics{
		successCount:  make(map[RenderType]int),
		failureCount:  make(map[RenderType]int),
		timeoutCount:  make(map[RenderType]int),
		totalDuration: make(map[RenderType]time.Duration),
	}
}

func (m *RenderMetrics) Record(renderType RenderType, duration time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.totalDuration[renderType] += duration

	if err == nil {
		m.successCount[renderType]++
	} else if err == ErrTimeout {
		m.timeoutCount[renderType]++
	} else {
		m.failureCount[renderType]++
	}
}

func (m *RenderMetrics) Print() {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Println("  렌더링 메트릭:")
	for _, rt := range []RenderType{RenderPNG, RenderPDF, RenderCSV} {
		s := m.successCount[rt]
		f := m.failureCount[rt]
		t := m.timeoutCount[rt]
		total := s + f + t
		if total > 0 {
			avgDur := m.totalDuration[rt] / time.Duration(total)
			fmt.Printf("    %s: 성공=%d, 실패=%d, 타임아웃=%d, 평균시간=%v\n",
				rt, s, f, t, avgDur)
		}
	}
}

// --- RenderingService 구현 ---

type RenderingService struct {
	available       bool
	version         string
	inProgressCount atomic.Int32
	renderKeyCache  map[string]*RenderUser
	keyProvider     *perRequestRenderKeyProvider
	capabilities    []Capability
	metrics         *RenderMetrics
	domain          string
	callbackURL     string
	rendererURL     string
}

func NewRenderingService(rendererURL, callbackURL string) *RenderingService {
	keyProvider := newPerRequestRenderKeyProvider()
	return &RenderingService{
		available:      rendererURL != "",
		version:        "3.10.0",
		renderKeyCache: keyProvider.cache,
		keyProvider:    keyProvider,
		capabilities: []Capability{
			{Name: FullHeightImages, MinMajor: 3, MinMinor: 4, MinPatch: 0},
			{Name: ScalingDownImages, MinMajor: 3, MinMinor: 4, MinPatch: 0},
			{Name: PDFRendering, MinMajor: 3, MinMinor: 10, MinPatch: 0},
		},
		metrics:     NewRenderMetrics(),
		domain:      "localhost",
		callbackURL: callbackURL,
		rendererURL: rendererURL,
	}
}

func (rs *RenderingService) IsAvailable() bool {
	return rs.available
}

// parseVersion은 간단한 semver 파서이다.
func parseVersion(v string) (int, int, int) {
	parts := [3]int{}
	idx := 0
	current := ""
	for _, c := range v + "." {
		if c == '.' {
			if idx < 3 {
				parts[idx], _ = strconv.Atoi(current)
				idx++
			}
			current = ""
		} else {
			current += string(c)
		}
	}
	return parts[0], parts[1], parts[2]
}

// HasCapability는 현재 렌더러 버전이 특정 기능을 지원하는지 확인한다.
func (rs *RenderingService) HasCapability(capability CapabilityName) (bool, string) {
	for _, cap := range rs.capabilities {
		if cap.Name == capability {
			major, minor, patch := parseVersion(rs.version)
			supported := (major > cap.MinMajor) ||
				(major == cap.MinMajor && minor > cap.MinMinor) ||
				(major == cap.MinMajor && minor == cap.MinMinor && patch >= cap.MinPatch)
			constraint := fmt.Sprintf(">= %d.%d.%d", cap.MinMajor, cap.MinMinor, cap.MinPatch)
			return supported, constraint
		}
	}
	return false, ""
}

// generateRendererURL은 Image Renderer에 보낼 URL을 생성한다.
func (rs *RenderingService) generateRendererURL(renderType RenderType, opts RenderOpts, renderKey string) string {
	baseURL := rs.rendererURL
	if renderType == RenderCSV {
		baseURL += "/csv"
	}

	u, _ := url.Parse(baseURL)
	q := u.Query()
	q.Add("url", fmt.Sprintf("%s%s&render=1", rs.callbackURL, opts.Path))
	q.Add("renderKey", renderKey)
	q.Add("domain", rs.domain)
	q.Add("timeout", strconv.Itoa(int(opts.Timeout.Seconds())))

	if renderType == RenderPNG {
		q.Add("width", strconv.Itoa(opts.Width))
		q.Add("height", strconv.Itoa(opts.Height))
	}

	scale := opts.DeviceScale
	if math.IsInf(scale, 0) || math.IsNaN(scale) || scale == 0 {
		scale = 1
	}
	if renderType != RenderCSV {
		q.Add("deviceScaleFactor", fmt.Sprintf("%.1f", scale))
	}

	u.RawQuery = q.Encode()
	return u.String()
}

// Render는 렌더링을 실행한다.
func (rs *RenderingService) Render(ctx context.Context, renderType RenderType, opts RenderOpts) (*RenderResult, error) {
	startTime := time.Now()

	result, err := rs.render(ctx, renderType, opts, rs.keyProvider)

	elapsedTime := time.Since(startTime)
	rs.metrics.Record(renderType, elapsedTime, err)

	return result, err
}

func (rs *RenderingService) render(ctx context.Context, renderType RenderType,
	opts RenderOpts, keyProvider renderKeyProvider) (*RenderResult, error) {

	// 1. 가용성 확인
	if !rs.IsAvailable() {
		return rs.renderUnavailableImage(), nil
	}

	// 2. 동시성 제한 확인
	newCount := rs.inProgressCount.Add(1)
	defer rs.inProgressCount.Add(-1)

	if opts.ConcurrentLimit > 0 && int(newCount) > opts.ConcurrentLimit {
		fmt.Printf("    [경고] 동시성 제한 초과: %d/%d\n", newCount, opts.ConcurrentLimit)
		return nil, ErrConcurrentLimitReached
	}

	// 3. PDF 기능 확인
	if renderType == RenderPDF {
		supported, constraint := rs.HasCapability(PDFRendering)
		if !supported {
			return nil, fmt.Errorf("PDF rendering requires %s", constraint)
		}
	}

	// 4. DeviceScaleFactor 정규화
	if math.IsInf(opts.DeviceScale, 0) || math.IsNaN(opts.DeviceScale) || opts.DeviceScale == 0 {
		opts.DeviceScale = 1
	}

	// 5. renderKey 획득
	renderKey, err := keyProvider.get(opts.AuthOpts)
	if err != nil {
		return nil, err
	}
	defer keyProvider.afterRequest(renderKey)

	// 6. 렌더러 URL 생성
	rendererURL := rs.generateRendererURL(renderType, opts, renderKey)
	fmt.Printf("    [RenderingService] 렌더링 요청: %s\n", truncateURL(rendererURL, 80))

	// 7. 시뮬레이션: 실제로는 HTTP 요청 후 파일 저장
	time.Sleep(30 * time.Millisecond)

	// 8. 결과 반환
	filePath := fmt.Sprintf("/tmp/grafana/render_%s.%s", generateRandomKey()[:8], renderType)
	fmt.Printf("    [RenderingService] 렌더링 완료: %s\n", filePath)

	return &RenderResult{FilePath: filePath}, nil
}

func (rs *RenderingService) renderUnavailableImage() *RenderResult {
	return &RenderResult{FilePath: "public/img/rendering_plugin_not_installed.png"}
}

func (rs *RenderingService) renderErrorImage(err error) *RenderResult {
	if err == ErrTimeout {
		return &RenderResult{FilePath: "public/img/rendering_timeout_dark.png"}
	}
	return &RenderResult{FilePath: "public/img/rendering_error_dark.png"}
}

// CreateRenderingSession은 세션 기반 렌더링을 위한 세션을 생성한다.
func (rs *RenderingService) CreateRenderingSession(opts AuthOpts, expiry time.Duration) *sessionRenderKeyProvider {
	return newSessionRenderKeyProvider(rs.renderKeyCache, opts, expiry)
}

// --- 유틸리티 ---

func generateRandomKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func truncateURL(u string, maxLen int) string {
	if len(u) <= maxLen {
		return u
	}
	return u[:maxLen] + "..."
}

// --- 메인 실행 ---

func main() {
	fmt.Println("=== Grafana 렌더링(Rendering) 시스템 PoC ===")
	fmt.Println()

	rs := NewRenderingService("http://renderer:8081/render", "http://grafana:3000/")

	// -------------------------------------------------------
	// 1. 기본 렌더링
	// -------------------------------------------------------
	fmt.Println("--- [1] PNG 렌더링 ---")

	result, err := rs.Render(context.Background(), RenderPNG, RenderOpts{
		Path:        "d/abc123/dashboard?orgId=1&panelId=2",
		Width:       1000,
		Height:      500,
		DeviceScale: 2.0,
		Theme:       "dark",
		Timeout:     30 * time.Second,
		AuthOpts: AuthOpts{
			OrgID:   1,
			UserID:  42,
			OrgRole: "Admin",
		},
	})
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
	} else {
		fmt.Printf("  결과 파일: %s\n", result.FilePath)
	}
	fmt.Println()

	// -------------------------------------------------------
	// 2. PDF 렌더링
	// -------------------------------------------------------
	fmt.Println("--- [2] PDF 렌더링 ---")

	result, err = rs.Render(context.Background(), RenderPDF, RenderOpts{
		Path:        "d/abc123/dashboard?orgId=1",
		Timeout:     60 * time.Second,
		AuthOpts: AuthOpts{
			OrgID:   1,
			UserID:  42,
			OrgRole: "Admin",
		},
	})
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
	} else {
		fmt.Printf("  결과 파일: %s\n", result.FilePath)
	}
	fmt.Println()

	// -------------------------------------------------------
	// 3. Capability 확인
	// -------------------------------------------------------
	fmt.Println("--- [3] Capability 시스템 ---")

	fmt.Printf("  렌더러 버전: %s\n", rs.version)

	caps := []CapabilityName{FullHeightImages, ScalingDownImages, PDFRendering}
	for _, cap := range caps {
		supported, constraint := rs.HasCapability(cap)
		fmt.Printf("  %s: 지원=%v (조건: %s)\n", cap, supported, constraint)
	}

	// 낮은 버전으로 테스트
	rs.version = "3.3.0"
	fmt.Printf("\n  렌더러 버전 변경: %s\n", rs.version)
	for _, cap := range caps {
		supported, constraint := rs.HasCapability(cap)
		fmt.Printf("  %s: 지원=%v (조건: %s)\n", cap, supported, constraint)
	}
	rs.version = "3.10.0" // 원복
	fmt.Println()

	// -------------------------------------------------------
	// 4. 동시성 제어
	// -------------------------------------------------------
	fmt.Println("--- [4] 동시성 제어 ---")

	var wg sync.WaitGroup
	concurrentLimit := 3
	successCount := 0
	failCount := 0
	var countMu sync.Mutex

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, err := rs.Render(context.Background(), RenderPNG, RenderOpts{
				Path:            fmt.Sprintf("d/test/panel?panelId=%d", id),
				Width:           800,
				Height:          400,
				ConcurrentLimit: concurrentLimit,
				Timeout:         5 * time.Second,
				AuthOpts: AuthOpts{
					OrgID:   1,
					UserID:  int64(id),
					OrgRole: "Viewer",
				},
			})
			countMu.Lock()
			if err != nil {
				failCount++
			} else {
				successCount++
			}
			countMu.Unlock()
		}(i)
	}
	wg.Wait()

	fmt.Printf("  동시성 제한: %d\n", concurrentLimit)
	fmt.Printf("  총 요청: 6, 성공: %d, 제한 초과: %d\n", successCount, failCount)
	fmt.Println()

	// -------------------------------------------------------
	// 5. renderKey 인증
	// -------------------------------------------------------
	fmt.Println("--- [5] renderKey 인증 ---")

	keyProvider := newPerRequestRenderKeyProvider()

	// 요청 1: 키 생성
	key1, _ := keyProvider.get(AuthOpts{OrgID: 1, UserID: 42, OrgRole: "Admin"})
	fmt.Printf("  renderKey 생성: %s...\n", key1[:16])

	// 키로 사용자 조회
	if user, ok := keyProvider.lookup(key1); ok {
		fmt.Printf("  renderKey 조회: OrgID=%d, UserID=%d, Role=%s\n",
			user.OrgID, user.UserID, user.OrgRole)
	}

	// 요청 완료 후 키 삭제
	keyProvider.afterRequest(key1)
	if _, ok := keyProvider.lookup(key1); !ok {
		fmt.Printf("  renderKey 삭제 확인: 키가 더 이상 유효하지 않음\n")
	}
	fmt.Println()

	// -------------------------------------------------------
	// 6. 세션 기반 렌더링
	// -------------------------------------------------------
	fmt.Println("--- [6] 세션 기반 렌더링 ---")

	session := rs.CreateRenderingSession(AuthOpts{
		OrgID:   1,
		UserID:  42,
		OrgRole: "Admin",
	}, 5*time.Minute)

	// 동일 세션으로 여러 패널 렌더링
	for i := 1; i <= 3; i++ {
		key, _ := session.get(AuthOpts{})
		fmt.Printf("  패널 %d 렌더링: renderKey=%s... (동일 키 재사용)\n", i, key[:16])
	}

	// 세션 종료
	session.dispose()
	fmt.Println("  세션 종료: renderKey 삭제됨")
	fmt.Println()

	// -------------------------------------------------------
	// 7. 에러 이미지 반환
	// -------------------------------------------------------
	fmt.Println("--- [7] 에러 이미지 ---")

	errImage := rs.renderErrorImage(ErrTimeout)
	fmt.Printf("  타임아웃 에러: %s\n", errImage.FilePath)

	errImage = rs.renderErrorImage(ErrRenderUnavailable)
	fmt.Printf("  일반 에러: %s\n", errImage.FilePath)

	unavailImage := rs.renderUnavailableImage()
	fmt.Printf("  렌더러 미설치: %s\n", unavailImage.FilePath)
	fmt.Println()

	// -------------------------------------------------------
	// 8. 렌더링 메트릭
	// -------------------------------------------------------
	fmt.Println("--- [8] 렌더링 메트릭 ---")
	rs.metrics.Print()
	fmt.Println()

	fmt.Println("=== 렌더링 시스템 PoC 완료 ===")
}
