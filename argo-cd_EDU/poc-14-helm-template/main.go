// poc-14-helm-template/main.go
//
// Argo CD Helm 통합 시뮬레이션
//
// 핵심 개념:
//   - Helm 인터페이스: Template(), GetParameters(), DependencyBuild()
//   - TemplateOpts: ReleaseName, Namespace, Values, SetParams
//   - Chart 구조: Chart.yaml, values.yaml, templates/
//   - 값 우선순위: chart defaults → user values → --set params
//   - DependencyBuild: 의존성 차트 해석
//   - manifestGenerateLock: 경로별 뮤텍스로 동시 빌드 방지
//   - Marker 파일: .argocd-helm-dep-up으로 중복 빌드 방지
//   - OCI 레지스트리: oci:// 프로토콜 처리
//
// 실행: go run main.go

package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"
)

// ============================================================
// Chart 구조 시뮬레이션
// ============================================================

// ChartDependency 차트 의존성 정보
type ChartDependency struct {
	Name       string
	Version    string
	Repository string // https:// 또는 oci://
	Alias      string
}

// ChartMetadata Chart.yaml 내용
type ChartMetadata struct {
	Name         string
	Version      string
	AppVersion   string
	Description  string
	Dependencies []ChartDependency
}

// Chart Helm 차트 전체 구조
type Chart struct {
	Metadata  ChartMetadata
	Values    map[string]interface{} // values.yaml
	Templates map[string]string      // templates/ 파일들 (파일명 → 내용)
}

// ============================================================
// TemplateOpts — 템플릿 렌더링 옵션
// ============================================================

// TemplateOpts Helm 템플릿 렌더링 옵션
// 실제 Argo CD: util/helm/cmd.go HelmTemplateOpts
type TemplateOpts struct {
	ReleaseName string
	Namespace   string
	// 사용자 values 파일 내용 (values.yaml 오버라이드)
	Values map[string]interface{}
	// --set 파라미터 (가장 높은 우선순위)
	SetParams map[string]string
	// --set-file 파라미터
	FileParams map[string]string
	// --kube-version
	KubeVersion string
	// --api-versions
	APIVersions []string
	// SkipCRDs
	SkipCRDs bool
}

// ============================================================
// Values 병합 — 우선순위 처리
// ============================================================

// mergeValues values를 우선순위에 따라 병합
// 우선순위 (낮음 → 높음): chart defaults → user values → --set params
func mergeValues(base map[string]interface{}, overrides ...map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	// 기본값 복사
	for k, v := range base {
		result[k] = v
	}
	// 오버라이드 적용
	for _, override := range overrides {
		for k, v := range override {
			if subMap, ok := v.(map[string]interface{}); ok {
				if existingMap, ok := result[k].(map[string]interface{}); ok {
					// 재귀적 병합
					result[k] = mergeValues(existingMap, subMap)
					continue
				}
			}
			result[k] = v
		}
	}
	return result
}

// applySetParams --set 파라미터를 values 맵에 적용
// "a.b.c=value" 형식을 중첩 맵으로 변환
func applySetParams(values map[string]interface{}, setParams map[string]string) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range values {
		result[k] = v
	}
	for key, val := range setParams {
		parts := strings.Split(key, ".")
		setNestedValue(result, parts, val)
	}
	return result
}

// setNestedValue 중첩 경로에 값 설정
func setNestedValue(m map[string]interface{}, keys []string, val string) {
	if len(keys) == 1 {
		m[keys[0]] = val
		return
	}
	if _, ok := m[keys[0]]; !ok {
		m[keys[0]] = make(map[string]interface{})
	}
	if sub, ok := m[keys[0]].(map[string]interface{}); ok {
		setNestedValue(sub, keys[1:], val)
	}
}

// flattenValues 중첩 맵을 "key.subkey=value" 형식으로 평탄화 (디버깅용)
func flattenValues(prefix string, m map[string]interface{}) []string {
	var result []string
	for k, v := range m {
		fullKey := k
		if prefix != "" {
			fullKey = prefix + "." + k
		}
		if subMap, ok := v.(map[string]interface{}); ok {
			result = append(result, flattenValues(fullKey, subMap)...)
		} else {
			result = append(result, fmt.Sprintf("%s=%v", fullKey, v))
		}
	}
	sort.Strings(result)
	return result
}

// ============================================================
// 템플릿 렌더링 컨텍스트
// ============================================================

// RenderContext Go 템플릿에 주입되는 컨텍스트
// 실제 Helm: .Release, .Chart, .Values, .Files, .Capabilities
type RenderContext struct {
	Release struct {
		Name      string
		Namespace string
		Service   string
		IsInstall bool
		IsUpgrade bool
		Revision  int
	}
	Chart  ChartMetadata
	Values map[string]interface{}
}

// ============================================================
// Helm 렌더링 엔진
// ============================================================

// RenderedManifest 렌더링된 K8s 매니페스트
type RenderedManifest struct {
	Filename string
	Content  string
}

// HelmEngine Helm 렌더링 엔진
type HelmEngine struct{}

// Template 차트를 템플릿 옵션으로 렌더링
func (e *HelmEngine) Template(chart *Chart, opts *TemplateOpts) ([]RenderedManifest, error) {
	// 1. Values 병합 (우선순위: chart → user → --set)
	merged := mergeValues(chart.Values, opts.Values)
	merged = applySetParams(merged, opts.SetParams)

	fmt.Println("  [HelmEngine] Values 병합:")
	for _, line := range flattenValues("", merged) {
		fmt.Printf("    %s\n", line)
	}

	// 2. 렌더 컨텍스트 구성
	ctx := &RenderContext{
		Chart:  chart.Metadata,
		Values: merged,
	}
	ctx.Release.Name = opts.ReleaseName
	ctx.Release.Namespace = opts.Namespace
	ctx.Release.Service = "Helm"
	ctx.Release.IsInstall = true
	ctx.Release.Revision = 1

	// 3. 각 템플릿 파일 렌더링
	var manifests []RenderedManifest
	// 파일명 순서를 일정하게 유지
	var filenames []string
	for name := range chart.Templates {
		filenames = append(filenames, name)
	}
	sort.Strings(filenames)

	for _, filename := range filenames {
		content := chart.Templates[filename]

		tmpl, err := template.New(filename).Funcs(helmFuncMap()).Parse(content)
		if err != nil {
			return nil, fmt.Errorf("템플릿 파싱 오류 [%s]: %w", filename, err)
		}

		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, ctx); err != nil {
			return nil, fmt.Errorf("템플릿 렌더링 오류 [%s]: %w", filename, err)
		}

		rendered := strings.TrimSpace(buf.String())
		if rendered != "" {
			manifests = append(manifests, RenderedManifest{
				Filename: filename,
				Content:  rendered,
			})
		}
	}

	return manifests, nil
}

// helmFuncMap Helm 호환 템플릿 함수 맵
func helmFuncMap() template.FuncMap {
	return template.FuncMap{
		"toYaml": func(v interface{}) string {
			return formatYAML(v, 0)
		},
		"indent": func(n int, s string) string {
			pad := strings.Repeat(" ", n)
			return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
		},
		"nindent": func(n int, s string) string {
			pad := strings.Repeat(" ", n)
			return "\n" + pad + strings.ReplaceAll(s, "\n", "\n"+pad)
		},
		"quote": func(s interface{}) string {
			return fmt.Sprintf("%q", fmt.Sprintf("%v", s))
		},
		"default": func(def interface{}, v interface{}) interface{} {
			if v == nil || v == "" || v == false || v == 0 {
				return def
			}
			return v
		},
		"required": func(msg string, v interface{}) (interface{}, error) {
			if v == nil || v == "" {
				return nil, fmt.Errorf(msg)
			}
			return v, nil
		},
		"include": func(name string, data interface{}) string {
			return "" // 단순화 — 실제에서는 named template 실행
		},
		"trunc": func(n int, s string) string {
			if len(s) > n {
				return s[:n]
			}
			return s
		},
		"trimSuffix": func(suffix, s string) string {
			return strings.TrimSuffix(s, suffix)
		},
		"lower": strings.ToLower,
		"upper": strings.ToUpper,
		"title": strings.Title,
	}
}

// formatYAML 간단한 YAML 직렬화 (toYaml 함수용)
func formatYAML(v interface{}, indent int) string {
	pad := strings.Repeat("  ", indent)
	switch val := v.(type) {
	case map[string]interface{}:
		var keys []string
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for _, k := range keys {
			sub := formatYAML(val[k], indent+1)
			if strings.Contains(sub, "\n") {
				sb.WriteString(fmt.Sprintf("%s%s:\n%s", pad, k, sub))
			} else {
				sb.WriteString(fmt.Sprintf("%s%s: %s\n", pad, k, strings.TrimSpace(sub)))
			}
		}
		return sb.String()
	case []interface{}:
		var sb strings.Builder
		for _, item := range val {
			sub := formatYAML(item, indent+1)
			sb.WriteString(fmt.Sprintf("%s- %s\n", pad, strings.TrimSpace(sub)))
		}
		return sb.String()
	case string:
		return fmt.Sprintf("%s\n", val)
	default:
		return fmt.Sprintf("%v\n", val)
	}
}

// ============================================================
// GetParameters — 차트 파라미터 조회
// ============================================================

// HelmParameter Helm 파라미터 (name=value)
type HelmParameter struct {
	Name  string
	Value string
}

// GetParameters 차트의 파라미터 목록 반환 (values.yaml 키 기반)
func GetParameters(chart *Chart, opts *TemplateOpts) []HelmParameter {
	merged := mergeValues(chart.Values, opts.Values)
	merged = applySetParams(merged, opts.SetParams)
	flat := flattenValues("", merged)
	var params []HelmParameter
	for _, line := range flat {
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		params = append(params, HelmParameter{
			Name:  line[:idx],
			Value: line[idx+1:],
		})
	}
	return params
}

// ============================================================
// DependencyBuild — 의존성 차트 처리
// ============================================================

// MarkerFile 의존성 빌드 완료 마커 파일 경로
func markerFilePath(chartPath string) string {
	return filepath.Join(chartPath, ".argocd-helm-dep-up")
}

// markerStore 마커 파일 인메모리 저장소 (실제: 파일시스템)
var markerStore = map[string]time.Time{}
var markerMu sync.Mutex

// markerExists 마커 파일 존재 여부 확인
func markerExists(path string) bool {
	markerMu.Lock()
	defer markerMu.Unlock()
	_, ok := markerStore[path]
	return ok
}

// writeMarker 마커 파일 생성
func writeMarker(path string) {
	markerMu.Lock()
	defer markerMu.Unlock()
	markerStore[path] = time.Now()
	fmt.Printf("  [Marker] 생성: %s\n", path)
}

// DependencyBuildResult 의존성 빌드 결과
type DependencyBuildResult struct {
	ChartPath    string
	Dependencies []string
	Skipped      bool
}

// DependencyBuild 차트 의존성 빌드
// 실제 Argo CD: util/helm/cmd.go DependencyBuild()
func DependencyBuild(chartPath string, chart *Chart) (*DependencyBuildResult, error) {
	marker := markerFilePath(chartPath)

	// 마커 파일이 있으면 건너뜀 (이미 의존성 빌드 완료)
	if markerExists(marker) {
		fmt.Printf("  [DependencyBuild] 마커 파일 존재 — 건너뜀: %s\n", chartPath)
		return &DependencyBuildResult{
			ChartPath: chartPath,
			Skipped:   true,
		}, nil
	}

	fmt.Printf("  [DependencyBuild] 의존성 빌드 시작: %s\n", chartPath)
	var resolved []string

	for _, dep := range chart.Metadata.Dependencies {
		if strings.HasPrefix(dep.Repository, "oci://") {
			// OCI 레지스트리에서 차트 추출
			resolved = append(resolved, resolveOCIDependency(dep))
		} else if strings.HasPrefix(dep.Repository, "https://") {
			// 표준 Helm 레포지토리에서 차트 다운로드
			resolved = append(resolved, resolveHTTPSDependency(dep))
		} else {
			// 로컬 의존성
			resolved = append(resolved, fmt.Sprintf("local:%s@%s", dep.Name, dep.Version))
		}
	}

	// 마커 파일 생성
	writeMarker(marker)

	return &DependencyBuildResult{
		ChartPath:    chartPath,
		Dependencies: resolved,
		Skipped:      false,
	}, nil
}

// resolveOCIDependency OCI 레지스트리 의존성 처리
func resolveOCIDependency(dep ChartDependency) string {
	// oci://registry.example.com/charts/postgresql:13.2.1
	ref := fmt.Sprintf("%s/%s:%s", dep.Repository, dep.Name, dep.Version)
	fmt.Printf("    [OCI] 추출: %s\n", ref)
	return fmt.Sprintf("oci:%s@%s", dep.Name, dep.Version)
}

// resolveHTTPSDependency HTTPS 레포지토리 의존성 처리
func resolveHTTPSDependency(dep ChartDependency) string {
	fmt.Printf("    [HTTP] 다운로드: %s/%s-%s.tgz\n", dep.Repository, dep.Name, dep.Version)
	return fmt.Sprintf("https:%s@%s", dep.Name, dep.Version)
}

// ============================================================
// manifestGenerateLock — 경로별 뮤텍스
// ============================================================
//
// 실제 Argo CD: reposerver/repository.go manifestGenerateLock
// 동일 경로에 대한 동시 `helm template` 실행을 방지.

type pathLockManager struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newPathLockManager() *pathLockManager {
	return &pathLockManager{locks: make(map[string]*sync.Mutex)}
}

func (m *pathLockManager) getLock(path string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.locks[path]; !ok {
		m.locks[path] = &sync.Mutex{}
	}
	return m.locks[path]
}

func (m *pathLockManager) Lock(path string) {
	m.getLock(path).Lock()
}

func (m *pathLockManager) Unlock(path string) {
	m.getLock(path).Unlock()
}

// globalLockManager 전역 경로별 잠금 관리자
var globalLockManager = newPathLockManager()

// TemplateWithLock 경로별 잠금 후 템플릿 렌더링
func TemplateWithLock(chartPath string, chart *Chart, opts *TemplateOpts) ([]RenderedManifest, error) {
	fmt.Printf("  [Lock] 경로 잠금 획득: %s\n", chartPath)
	globalLockManager.Lock(chartPath)
	defer func() {
		globalLockManager.Unlock(chartPath)
		fmt.Printf("  [Lock] 경로 잠금 해제: %s\n", chartPath)
	}()

	engine := &HelmEngine{}
	return engine.Template(chart, opts)
}

// ============================================================
// 샘플 차트 정의
// ============================================================

// makeWebappChart 웹 애플리케이션 차트 생성
func makeWebappChart() *Chart {
	return &Chart{
		Metadata: ChartMetadata{
			Name:        "webapp",
			Version:     "1.2.0",
			AppVersion:  "2.5.1",
			Description: "A simple web application Helm chart",
			Dependencies: []ChartDependency{
				{
					Name:       "postgresql",
					Version:    "13.2.1",
					Repository: "oci://registry-1.docker.io/bitnamicharts",
				},
				{
					Name:       "redis",
					Version:    "18.1.5",
					Repository: "https://charts.bitnami.com/bitnami",
				},
			},
		},
		Values: map[string]interface{}{
			"replicaCount": 1,
			"image": map[string]interface{}{
				"repository": "nginx",
				"tag":        "1.21",
				"pullPolicy": "IfNotPresent",
			},
			"service": map[string]interface{}{
				"type": "ClusterIP",
				"port": 80,
			},
			"resources": map[string]interface{}{
				"limits": map[string]interface{}{
					"cpu":    "100m",
					"memory": "128Mi",
				},
				"requests": map[string]interface{}{
					"cpu":    "50m",
					"memory": "64Mi",
				},
			},
			"autoscaling": map[string]interface{}{
				"enabled":                        false,
				"minReplicas":                    1,
				"maxReplicas":                    10,
				"targetCPUUtilizationPercentage": 80,
			},
		},
		Templates: map[string]string{
			"deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-webapp
  namespace: {{ .Release.Namespace }}
  labels:
    app.kubernetes.io/name: webapp
    app.kubernetes.io/instance: {{ .Release.Name }}
    app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels:
      app.kubernetes.io/name: webapp
      app.kubernetes.io/instance: {{ .Release.Name }}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: webapp
        app.kubernetes.io/instance: {{ .Release.Name }}
    spec:
      containers:
        - name: webapp
          image: "{{ index .Values "image" "repository" }}:{{ index .Values "image" "tag" }}"
          imagePullPolicy: {{ index .Values "image" "pullPolicy" }}
          ports:
            - name: http
              containerPort: 80
              protocol: TCP`,
			"service.yaml": `apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-webapp
  namespace: {{ .Release.Namespace }}
  labels:
    app.kubernetes.io/name: webapp
    app.kubernetes.io/instance: {{ .Release.Name }}
spec:
  type: {{ index .Values "service" "type" }}
  ports:
    - port: {{ index .Values "service" "port" }}
      targetPort: http
      protocol: TCP
      name: http
  selector:
    app.kubernetes.io/name: webapp
    app.kubernetes.io/instance: {{ .Release.Name }}`,
			"configmap.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-webapp-config
  namespace: {{ .Release.Namespace }}
data:
  app.version: {{ .Chart.AppVersion | quote }}
  release.name: {{ .Release.Name | quote }}`,
		},
	}
}

// ============================================================
// 시뮬레이션 실행
// ============================================================

func main() {
	fmt.Println("============================================================")
	fmt.Println("  Argo CD Helm 통합 시뮬레이션 (PoC-14)")
	fmt.Println("============================================================")

	chart := makeWebappChart()
	chartPath := "/tmp/argocd/repos/myorg/webapp"
	engine := &HelmEngine{}

	// ----------------------------------------------------------------
	// 시나리오 1: 기본 템플릿 렌더링
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 1: 기본 Helm 템플릿 렌더링")
	fmt.Println("============================")

	opts1 := &TemplateOpts{
		ReleaseName: "myapp",
		Namespace:   "production",
		Values:      map[string]interface{}{},
		SetParams:   map[string]string{},
	}

	manifests1, err := engine.Template(chart, opts1)
	if err != nil {
		fmt.Printf("오류: %v\n", err)
		return
	}
	fmt.Printf("\n렌더링된 매니페스트 수: %d\n", len(manifests1))
	for _, m := range manifests1 {
		fmt.Printf("\n--- 파일: %s ---\n%s\n", m.Filename, m.Content)
	}

	// ----------------------------------------------------------------
	// 시나리오 2: Values 오버라이드 및 --set 파라미터
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 2: Values 우선순위 (chart → user values → --set)")
	fmt.Println("============================")

	opts2 := &TemplateOpts{
		ReleaseName: "prod-webapp",
		Namespace:   "production",
		Values: map[string]interface{}{
			"replicaCount": 3,
			"image": map[string]interface{}{
				"repository": "myorg/webapp",
				"tag":        "v2.5.1",
			},
			"service": map[string]interface{}{
				"type": "LoadBalancer",
			},
		},
		// --set 파라미터 (가장 높은 우선순위)
		SetParams: map[string]string{
			"image.tag":            "v2.5.1-hotfix",
			"autoscaling.enabled":  "true",
			"autoscaling.maxReplicas": "20",
		},
	}

	manifests2, err := engine.Template(chart, opts2)
	if err != nil {
		fmt.Printf("오류: %v\n", err)
		return
	}
	fmt.Printf("\n렌더링된 매니페스트 수: %d\n", len(manifests2))
	// Deployment만 출력
	for _, m := range manifests2 {
		if m.Filename == "deployment.yaml" {
			fmt.Printf("\n--- %s ---\n%s\n", m.Filename, m.Content)
		}
	}

	// ----------------------------------------------------------------
	// 시나리오 3: GetParameters
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 3: GetParameters — 유효 파라미터 목록")
	fmt.Println("============================")

	params := GetParameters(chart, opts2)
	fmt.Printf("파라미터 수: %d\n", len(params))
	for _, p := range params {
		fmt.Printf("  %-45s = %s\n", p.Name, p.Value)
	}

	// ----------------------------------------------------------------
	// 시나리오 4: DependencyBuild (마커 파일 방지)
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 4: DependencyBuild — 의존성 해석 및 마커 파일")
	fmt.Println("============================")

	fmt.Println("\n[1차 빌드] 의존성 다운로드:")
	result1, err := DependencyBuild(chartPath, chart)
	if err != nil {
		fmt.Printf("오류: %v\n", err)
		return
	}
	fmt.Printf("  경로: %s\n", result1.ChartPath)
	fmt.Printf("  건너뜀: %v\n", result1.Skipped)
	fmt.Printf("  해석된 의존성: %v\n", result1.Dependencies)

	fmt.Println("\n[2차 빌드] 마커 파일 존재 → 건너뜀:")
	result2, _ := DependencyBuild(chartPath, chart)
	fmt.Printf("  건너뜀: %v\n", result2.Skipped)

	// ----------------------------------------------------------------
	// 시나리오 5: manifestGenerateLock (동시 렌더링 방지)
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 5: manifestGenerateLock — 경로별 동시 렌더링 방지")
	fmt.Println("============================")

	var wg sync.WaitGroup
	paths := []string{"/charts/app-a", "/charts/app-b", "/charts/app-a"}
	opts3 := &TemplateOpts{
		ReleaseName: "test",
		Namespace:   "default",
		Values:      map[string]interface{}{},
		SetParams:   map[string]string{},
	}

	for i, path := range paths {
		wg.Add(1)
		go func(idx int, p string) {
			defer wg.Done()
			// 랜덤 지연으로 동시성 시뮬레이션
			time.Sleep(time.Duration(rand.Intn(50)) * time.Millisecond)
			fmt.Printf("  [고루틴 %d] %s 렌더링 시작\n", idx, p)
			_, err := TemplateWithLock(p, chart, opts3)
			if err != nil {
				fmt.Printf("  [고루틴 %d] 오류: %v\n", idx, err)
				return
			}
			fmt.Printf("  [고루틴 %d] %s 렌더링 완료\n", idx, p)
		}(i, path)
	}
	wg.Wait()
	fmt.Println("  모든 고루틴 완료 — app-a 경로는 순차 처리됨")

	// ----------------------------------------------------------------
	// 시나리오 6: OCI 레지스트리 의존성
	// ----------------------------------------------------------------
	fmt.Println("\n============================")
	fmt.Println("시나리오 6: OCI 레지스트리 차트 의존성 처리")
	fmt.Println("============================")

	ociChart := &Chart{
		Metadata: ChartMetadata{
			Name:    "oci-app",
			Version: "0.1.0",
			Dependencies: []ChartDependency{
				{
					Name:       "postgresql",
					Version:    "15.0.0",
					Repository: "oci://ghcr.io/bitnami/charts",
				},
				{
					Name:       "monitoring",
					Version:    "2.0.0",
					Repository: "oci://registry.myorg.com/helm-charts",
				},
			},
		},
		Values:    map[string]interface{}{},
		Templates: map[string]string{},
	}

	ociChartPath := "/tmp/argocd/repos/myorg/oci-app"
	ociResult, err := DependencyBuild(ociChartPath, ociChart)
	if err != nil {
		fmt.Printf("오류: %v\n", err)
		return
	}
	fmt.Printf("  OCI 의존성 해석 결과:\n")
	for _, dep := range ociResult.Dependencies {
		fmt.Printf("    %s\n", dep)
	}

	fmt.Println("\n[완료] Helm 통합 시뮬레이션 종료")
}
