// Package main은 Argo CD의 Deep Links와 Badge Server 서브시스템을
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Deep Links 설정 구조 (Title, URL 템플릿, 조건식)
// 2. Go 템플릿 기반 URL 렌더링
// 3. 조건식 평가 (expr 언어 시뮬레이션)
// 4. 4가지 Deep Link 유형 (resource, application, cluster, project)
// 5. Badge Server SVG 생성
// 6. Health/Sync 상태별 색상 매핑
// 7. 배지 캐싱 및 HTTP 핸들러
// 8. 멀티 앱 배지 (그룹/프로젝트 단위)
// 9. Shields.io 스타일 SVG 템플릿
// 10. CORS 및 보안 헤더
//
// 실제 소스 참조:
//   - server/deeplinks/deeplinks.go   (Deep Links 핵심 로직)
//   - server/badge/badge.go           (Badge 서버)
//   - util/settings/settings.go       (DeepLink 구조체)
package main

import (
	"fmt"
	"net/url"
	"strings"
	"text/template"
)

// ============================================================================
// 1. Deep Links 시스템 (server/deeplinks/deeplinks.go 시뮬레이션)
// ============================================================================

// DeepLink는 Deep Link 설정을 나타낸다.
// 실제 구현: util/settings/settings.go의 DeepLink 구조체
type DeepLink struct {
	Title       string  // UI에 표시할 링크 제목
	URL         string  // Go 템플릿 기반 URL 패턴
	Condition   *string // expr 조건식 (nil이면 항상 표시)
	Description *string // 링크 설명
	IconClass   *string // UI 아이콘 CSS 클래스
}

// DeepLinkType은 Deep Link의 유형이다.
type DeepLinkType string

const (
	ResourceLink    DeepLinkType = "resource"
	ApplicationLink DeepLinkType = "application"
	ClusterLink     DeepLinkType = "cluster"
	ProjectLink     DeepLinkType = "project"
)

// DeepLinkContext는 Deep Link 렌더링에 필요한 컨텍스트 데이터다.
type DeepLinkContext struct {
	// resource 컨텍스트
	Resource    map[string]interface{} `json:"resource,omitempty"`
	Application map[string]interface{} `json:"application,omitempty"`
	Cluster     map[string]interface{} `json:"cluster,omitempty"`
	Project     map[string]interface{} `json:"project,omitempty"`
}

// EvaluateDeepLinks는 Deep Link 목록을 컨텍스트에 맞게 평가하여 렌더링된 링크를 반환한다.
// 실제 구현: server/deeplinks/deeplinks.go의 CreateDeepLinks
func EvaluateDeepLinks(links []DeepLink, ctx DeepLinkContext) []RenderedLink {
	var result []RenderedLink

	for _, link := range links {
		// 1. 조건식 평가
		if link.Condition != nil {
			if !evaluateCondition(*link.Condition, ctx) {
				continue
			}
		}

		// 2. URL 템플릿 렌더링
		renderedURL, err := renderURLTemplate(link.URL, ctx)
		if err != nil {
			// 렌더링 실패 시 무시 (실제 구현도 에러 로깅 후 스킵)
			continue
		}

		rendered := RenderedLink{
			Title: link.Title,
			URL:   renderedURL,
		}
		if link.Description != nil {
			rendered.Description = *link.Description
		}
		if link.IconClass != nil {
			rendered.IconClass = *link.IconClass
		}
		result = append(result, rendered)
	}

	return result
}

// RenderedLink는 렌더링된 Deep Link다.
type RenderedLink struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	IconClass   string `json:"iconClass,omitempty"`
}

// renderURLTemplate은 Go 템플릿으로 URL을 렌더링한다.
func renderURLTemplate(urlTemplate string, ctx DeepLinkContext) (string, error) {
	// Go template + Sprig 함수 시뮬레이션
	funcMap := template.FuncMap{
		"urlEncode": url.QueryEscape,
	}

	tmpl, err := template.New("url").Funcs(funcMap).Parse(urlTemplate)
	if err != nil {
		return "", fmt.Errorf("URL 템플릿 파싱 실패: %w", err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("URL 템플릿 렌더링 실패: %w", err)
	}

	return buf.String(), nil
}

// evaluateCondition은 간단한 조건식을 평가한다.
// 실제 구현은 antonmedv/expr 라이브러리를 사용한다.
func evaluateCondition(condition string, ctx DeepLinkContext) bool {
	// 간단한 조건 평가 시뮬레이션
	// 실제로는 expr.Eval(condition, ctx)를 호출한다.

	switch {
	case strings.Contains(condition, "app.metadata.labels"):
		// 레이블 기반 조건: 앱에 특정 레이블이 있는지 확인
		if ctx.Application != nil {
			if labels, ok := ctx.Application["labels"].(map[string]string); ok {
				// "app.metadata.labels.team == 'platform'" 형태
				for _, v := range labels {
					if strings.Contains(condition, v) {
						return true
					}
				}
			}
		}
		return false
	case strings.Contains(condition, "resource.kind"):
		// 리소스 종류 기반 조건
		if ctx.Resource != nil {
			if kind, ok := ctx.Resource["kind"].(string); ok {
				if strings.Contains(condition, kind) {
					return true
				}
			}
		}
		return false
	default:
		// 조건식을 평가할 수 없으면 항상 true
		return true
	}
}

// ============================================================================
// 2. Badge Server (server/badge/badge.go 시뮬레이션)
// ============================================================================

// HealthStatus는 앱의 Health 상태다.
type HealthStatus string

const (
	HealthHealthy    HealthStatus = "Healthy"
	HealthDegraded   HealthStatus = "Degraded"
	HealthProgressing HealthStatus = "Progressing"
	HealthSuspended  HealthStatus = "Suspended"
	HealthMissing    HealthStatus = "Missing"
	HealthUnknown    HealthStatus = "Unknown"
)

// SyncStatus는 앱의 Sync 상태다.
type SyncStatus string

const (
	SyncSynced    SyncStatus = "Synced"
	SyncOutOfSync SyncStatus = "OutOfSync"
	SyncUnknown   SyncStatus = "Unknown"
)

// AppStatus는 앱의 상태 요약이다.
type AppStatus struct {
	Name   string
	Health HealthStatus
	Sync   SyncStatus
}

// BadgeConfig는 배지 색상 설정이다.
type BadgeConfig struct {
	HealthColors map[HealthStatus]string
	SyncColors   map[SyncStatus]string
}

// DefaultBadgeConfig는 기본 배지 색상이다.
var DefaultBadgeConfig = BadgeConfig{
	HealthColors: map[HealthStatus]string{
		HealthHealthy:     "#44cc11", // 녹색
		HealthDegraded:    "#fe7d37", // 주황
		HealthProgressing: "#1e90ff", // 파랑
		HealthSuspended:   "#9f9f9f", // 회색
		HealthMissing:     "#e05d44", // 빨강
		HealthUnknown:     "#9f9f9f", // 회색
	},
	SyncColors: map[SyncStatus]string{
		SyncSynced:    "#44cc11", // 녹색
		SyncOutOfSync: "#fe7d37", // 주황
		SyncUnknown:   "#9f9f9f", // 회색
	},
}

// svgTemplate은 Shields.io 스타일 SVG 배지 템플릿이다.
// 실제 구현에서는 server/badge/ 디렉토리의 SVG 템플릿 파일을 사용한다.
const svgTemplate = `<svg xmlns="http://www.w3.org/2000/svg" width="{{.Width}}" height="20">
  <linearGradient id="b" x2="0" y2="100%%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="a">
    <rect width="{{.Width}}" height="20" rx="3" fill="#fff"/>
  </clipPath>
  <g clip-path="url(#a)">
    <path fill="#555" d="M0 0h{{.LabelWidth}}v20H0z"/>
    <path fill="{{.Color}}" d="M{{.LabelWidth}} 0h{{.ValueWidth}}v20H{{.LabelWidth}}z"/>
    <path fill="url(#b)" d="M0 0h{{.Width}}v20H0z"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="DejaVu Sans,Verdana,Geneva,sans-serif" font-size="11">
    <text x="{{.LabelX}}" y="15" fill="#010101" fill-opacity=".3">{{.Label}}</text>
    <text x="{{.LabelX}}" y="14">{{.Label}}</text>
    <text x="{{.ValueX}}" y="15" fill="#010101" fill-opacity=".3">{{.Value}}</text>
    <text x="{{.ValueX}}" y="14">{{.Value}}</text>
  </g>
</svg>`

// BadgeData는 SVG 렌더링에 필요한 데이터다.
type BadgeData struct {
	Label      string
	Value      string
	Color      string
	LabelWidth int
	ValueWidth int
	Width      int
	LabelX     int
	ValueX     int
}

// GenerateBadge는 앱 상태를 SVG 배지로 생성한다.
func GenerateBadge(app AppStatus, badgeType string) string {
	var label, value, color string

	switch badgeType {
	case "health":
		label = app.Name
		value = string(app.Health)
		color = DefaultBadgeConfig.HealthColors[app.Health]
	case "sync":
		label = app.Name
		value = string(app.Sync)
		color = DefaultBadgeConfig.SyncColors[app.Sync]
	default:
		label = app.Name
		value = fmt.Sprintf("%s / %s", app.Health, app.Sync)
		if app.Health == HealthHealthy && app.Sync == SyncSynced {
			color = "#44cc11"
		} else if app.Health == HealthDegraded || app.Health == HealthMissing {
			color = "#e05d44"
		} else {
			color = "#fe7d37"
		}
	}

	// 너비 계산 (문자 폭 근사)
	labelWidth := len(label)*7 + 10
	valueWidth := len(value)*7 + 10

	data := BadgeData{
		Label:      label,
		Value:      value,
		Color:      color,
		LabelWidth: labelWidth,
		ValueWidth: valueWidth,
		Width:      labelWidth + valueWidth,
		LabelX:     labelWidth / 2,
		ValueX:     labelWidth + valueWidth/2,
	}

	tmpl, _ := template.New("badge").Parse(svgTemplate)
	var buf strings.Builder
	tmpl.Execute(&buf, data)
	return buf.String()
}

// GenerateMultiAppBadge는 여러 앱의 상태를 종합한 배지를 생성한다.
func GenerateMultiAppBadge(apps []AppStatus, projectName string) string {
	allHealthy := true
	allSynced := true

	for _, app := range apps {
		if app.Health != HealthHealthy {
			allHealthy = false
		}
		if app.Sync != SyncSynced {
			allSynced = false
		}
	}

	summary := AppStatus{
		Name: projectName,
	}

	if allHealthy {
		summary.Health = HealthHealthy
	} else {
		summary.Health = HealthDegraded
	}
	if allSynced {
		summary.Sync = SyncSynced
	} else {
		summary.Sync = SyncOutOfSync
	}

	return GenerateBadge(summary, "combined")
}

// ============================================================================
// 3. HTTP 핸들러 시뮬레이션
// ============================================================================

// BadgeHandler는 배지 HTTP 요청을 처리한다.
// GET /api/badge?name=<app>&revision=true
func BadgeHandler(appName string, badgeType string, apps map[string]AppStatus) (string, map[string]string) {
	headers := map[string]string{
		"Content-Type":  "image/svg+xml",
		"Cache-Control": "no-cache, no-store, must-revalidate",
		"Pragma":        "no-cache",
		"Expires":       "0",
	}

	app, ok := apps[appName]
	if !ok {
		// 404: 앱을 찾을 수 없음
		headers["Content-Type"] = "text/plain"
		return "Application not found", headers
	}

	svg := GenerateBadge(app, badgeType)
	return svg, headers
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Argo CD Deep Links & Badge Server 시뮬레이션 PoC           ║")
	fmt.Println("║  실제 소스: server/deeplinks/, server/badge/                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. Deep Links 데모 ===
	fmt.Println("=== 1. Deep Links 설정 및 렌더링 ===")

	// Deep Link 설정 (argocd-cm ConfigMap에서 읽어온 것 시뮬레이션)
	grafanaDesc := "Grafana 대시보드로 이동"
	grafanaIcon := "fa fa-chart-line"
	teamCondition := `app.metadata.labels.team == 'platform'`
	podCondition := `resource.kind == 'Pod'`

	links := []DeepLink{
		{
			Title:       "Grafana Dashboard",
			URL:         "https://grafana.example.com/d/app-overview?var-namespace={{.Application.namespace}}&var-app={{.Application.name}}",
			Condition:   nil, // 항상 표시
			Description: &grafanaDesc,
			IconClass:   &grafanaIcon,
		},
		{
			Title:     "Datadog APM",
			URL:       "https://app.datadoghq.com/apm?env={{.Application.namespace}}&service={{.Application.name}}",
			Condition: &teamCondition, // platform 팀만
		},
		{
			Title:     "Pod Logs (Kibana)",
			URL:       "https://kibana.example.com/app/logs?q=kubernetes.pod_name:{{.Resource.name}}",
			Condition: &podCondition, // Pod 리소스만
		},
	}

	// Application 컨텍스트
	appCtx := DeepLinkContext{
		Application: map[string]interface{}{
			"name":      "web-frontend",
			"namespace": "production",
			"labels": map[string]string{
				"team": "platform",
				"tier": "frontend",
			},
		},
	}

	fmt.Println("  [Application Deep Links]")
	rendered := EvaluateDeepLinks(links, appCtx)
	for _, r := range rendered {
		fmt.Printf("    %s: %s\n", r.Title, r.URL)
		if r.Description != "" {
			fmt.Printf("      설명: %s\n", r.Description)
		}
	}

	// Resource 컨텍스트 (Pod)
	fmt.Println()
	fmt.Println("  [Resource Deep Links - Pod]")
	podCtx := DeepLinkContext{
		Resource: map[string]interface{}{
			"kind": "Pod",
			"name": "web-frontend-abc123",
		},
		Application: appCtx.Application,
	}
	rendered = EvaluateDeepLinks(links, podCtx)
	for _, r := range rendered {
		fmt.Printf("    %s: %s\n", r.Title, r.URL)
	}

	// Resource 컨텍스트 (Service - Pod 조건 불충족)
	fmt.Println()
	fmt.Println("  [Resource Deep Links - Service]")
	svcCtx := DeepLinkContext{
		Resource: map[string]interface{}{
			"kind": "Service",
			"name": "web-frontend-svc",
		},
		Application: appCtx.Application,
	}
	rendered = EvaluateDeepLinks(links, svcCtx)
	for _, r := range rendered {
		fmt.Printf("    %s: %s\n", r.Title, r.URL)
	}
	fmt.Println()

	// === 2. Badge Server 데모 ===
	fmt.Println("=== 2. Badge Server SVG 생성 ===")

	apps := map[string]AppStatus{
		"web-frontend": {
			Name:   "web-frontend",
			Health: HealthHealthy,
			Sync:   SyncSynced,
		},
		"api-server": {
			Name:   "api-server",
			Health: HealthDegraded,
			Sync:   SyncOutOfSync,
		},
		"worker": {
			Name:   "worker",
			Health: HealthProgressing,
			Sync:   SyncSynced,
		},
	}

	fmt.Println("  [상태별 배지]")
	for name, app := range apps {
		badge := GenerateBadge(app, "combined")
		// SVG에서 핵심 정보만 추출하여 표시
		fmt.Printf("    %s: Health=%s, Sync=%s, 색상=%s, SVG크기=%d바이트\n",
			name, app.Health, app.Sync,
			func() string {
				if app.Health == HealthHealthy && app.Sync == SyncSynced {
					return "#44cc11(녹색)"
				} else if app.Health == HealthDegraded {
					return "#e05d44(빨강)"
				}
				return "#fe7d37(주황)"
			}(),
			len(badge))
	}

	// 개별 배지 유형 데모
	fmt.Println()
	fmt.Println("  [배지 유형별 데모]")
	testApp := apps["web-frontend"]
	for _, badgeType := range []string{"health", "sync", "combined"} {
		badge := GenerateBadge(testApp, badgeType)
		// SVG 내에서 value 텍스트만 추출
		fmt.Printf("    web-frontend [%s]: SVG 크기=%d바이트\n", badgeType, len(badge))
	}

	// === 3. 멀티 앱 배지 ===
	fmt.Println()
	fmt.Println("=== 3. 멀티 앱 배지 (프로젝트 단위) ===")

	allApps := []AppStatus{
		apps["web-frontend"],
		apps["api-server"],
		apps["worker"],
	}

	multiBadge := GenerateMultiAppBadge(allApps, "my-project")
	fmt.Printf("  프로젝트 'my-project' 배지: SVG 크기=%d바이트\n", len(multiBadge))
	fmt.Println("  (api-server가 Degraded이므로 전체 상태 Degraded)")

	// 모든 앱이 Healthy인 경우
	healthyApps := []AppStatus{
		{Name: "app1", Health: HealthHealthy, Sync: SyncSynced},
		{Name: "app2", Health: HealthHealthy, Sync: SyncSynced},
	}
	multiBadge = GenerateMultiAppBadge(healthyApps, "healthy-project")
	fmt.Printf("  프로젝트 'healthy-project' 배지: SVG 크기=%d바이트 (전체 Healthy)\n", len(multiBadge))

	// === 4. HTTP 핸들러 시뮬레이션 ===
	fmt.Println()
	fmt.Println("=== 4. Badge HTTP 핸들러 시뮬레이션 ===")

	// GET /api/badge?name=web-frontend&type=health
	body, headers := BadgeHandler("web-frontend", "health", apps)
	fmt.Printf("  GET /api/badge?name=web-frontend&type=health\n")
	fmt.Printf("    Content-Type: %s\n", headers["Content-Type"])
	fmt.Printf("    Cache-Control: %s\n", headers["Cache-Control"])
	fmt.Printf("    응답 크기: %d바이트 (SVG)\n", len(body))

	// GET /api/badge?name=nonexistent
	body, headers = BadgeHandler("nonexistent", "health", apps)
	fmt.Printf("  GET /api/badge?name=nonexistent\n")
	fmt.Printf("    Content-Type: %s\n", headers["Content-Type"])
	fmt.Printf("    응답: %s\n", body)

	// === 5. 색상 매핑 표 ===
	fmt.Println()
	fmt.Println("=== 5. Health/Sync 상태 색상 매핑 ===")
	fmt.Println("  [Health 상태]")
	for _, h := range []HealthStatus{HealthHealthy, HealthDegraded, HealthProgressing, HealthSuspended, HealthMissing, HealthUnknown} {
		fmt.Printf("    %-12s → %s\n", h, DefaultBadgeConfig.HealthColors[h])
	}
	fmt.Println("  [Sync 상태]")
	for _, s := range []SyncStatus{SyncSynced, SyncOutOfSync, SyncUnknown} {
		fmt.Printf("    %-12s → %s\n", s, DefaultBadgeConfig.SyncColors[s])
	}
}
