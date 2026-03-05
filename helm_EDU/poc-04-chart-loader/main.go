// Helm v4 Chart 로더 PoC: Chart.yaml 파싱, 디렉토리→Chart 변환, 의존성 트리
//
// 이 PoC는 Helm v4의 차트 로딩 매커니즘을 시뮬레이션합니다:
//   1. Chart.yaml 파싱 (pkg/chart/loader/load.go)
//   2. 디렉토리 구조 → Chart 구조체 변환 (pkg/chart/v2/loader/)
//   3. API 버전 감지 (v1/v2/v3) 및 적절한 로더 선택
//   4. 의존성 트리 구성 (charts/ 하위 디렉토리)
//   5. .helmignore 패턴 매칭
//
// 실행: go run main.go

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// =============================================================================
// 데이터 모델 (간소화)
// =============================================================================

type File struct {
	Name string
	Data []byte
}

type Dependency struct {
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	Repository string `json:"repository,omitempty"`
	Alias      string `json:"alias,omitempty"`
	Condition  string `json:"condition,omitempty"`
}

type Metadata struct {
	APIVersion   string        `json:"apiVersion"`
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	Description  string        `json:"description,omitempty"`
	Type         string        `json:"type,omitempty"`
	AppVersion   string        `json:"appVersion,omitempty"`
	Dependencies []*Dependency `json:"dependencies,omitempty"`
}

func (md *Metadata) Validate() error {
	if md.APIVersion == "" {
		return fmt.Errorf("chart.metadata.apiVersion 필수")
	}
	if md.Name == "" {
		return fmt.Errorf("chart.metadata.name 필수")
	}
	if md.Version == "" {
		return fmt.Errorf("chart.metadata.version 필수")
	}
	return nil
}

type Chart struct {
	Metadata     *Metadata      `json:"metadata"`
	Templates    []*File        `json:"-"`
	Values       map[string]any `json:"values,omitempty"`
	Files        []*File        `json:"-"`
	parent       *Chart
	dependencies []*Chart
}

func (ch *Chart) Name() string {
	if ch.Metadata == nil {
		return ""
	}
	return ch.Metadata.Name
}

func (ch *Chart) AddDependency(charts ...*Chart) {
	for _, c := range charts {
		c.parent = ch
		ch.dependencies = append(ch.dependencies, c)
	}
}

func (ch *Chart) Dependencies() []*Chart {
	return ch.dependencies
}

func (ch *Chart) IsRoot() bool { return ch.parent == nil }

// =============================================================================
// ChartLoader 인터페이스: Helm의 pkg/chart/loader/load.go
// 디렉토리 또는 아카이브 파일을 감지하여 적절한 로더 선택.
// =============================================================================

// ChartLoader는 차트를 로드하는 인터페이스이다.
// 실제 Helm: loader.ChartLoader interface { Load() (chart.Charter, error) }
type ChartLoader interface {
	Load() (*Chart, error)
}

// DirLoader는 디렉토리에서 차트를 로드한다.
// 실제 Helm: loader.DirLoader
type DirLoader struct {
	path string
}

// FileLoader는 아카이브 파일에서 차트를 로드한다.
// 실제 Helm: loader.FileLoader
type FileLoader struct {
	path string
}

// Loader는 경로를 검사하여 적절한 ChartLoader를 반환한다.
// 실제 Helm: loader.Loader(name string) (ChartLoader, error)
// → os.Stat로 디렉토리/파일 구분 → DirLoader 또는 FileLoader
func Loader(path string) (ChartLoader, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("차트 경로를 찾을 수 없습니다: %w", err)
	}
	if fi.IsDir() {
		return &DirLoader{path: path}, nil
	}
	return &FileLoader{path: path}, nil
}

// Load는 차트를 로드하는 편의 함수이다.
// 실제 Helm: loader.Load(name string) (chart.Charter, error)
func Load(path string) (*Chart, error) {
	l, err := Loader(path)
	if err != nil {
		return nil, err
	}
	return l.Load()
}

// =============================================================================
// DirLoader 구현: 디렉토리에서 차트 로드
// 실제 Helm: loader.LoadDir(dir string) → API 버전 감지 → c2load.Load 또는 c3load.Load
// =============================================================================

func (l *DirLoader) Load() (*Chart, error) {
	topdir, err := filepath.Abs(l.path)
	if err != nil {
		return nil, err
	}

	// 1) Chart.yaml 읽기 및 파싱
	// 실제 Helm: os.ReadFile(filepath.Join(topdir, "Chart.yaml"))
	chartYamlPath := filepath.Join(topdir, "Chart.yaml")
	data, err := os.ReadFile(chartYamlPath)
	if err != nil {
		return nil, fmt.Errorf("Chart.yaml를 찾을 수 없습니다: %s: %w", chartYamlPath, err)
	}

	// 2) API 버전 감지
	// 실제 Helm: chartBase 구조체로 apiVersion만 먼저 파싱
	metadata, err := parseChartYaml(data)
	if err != nil {
		return nil, fmt.Errorf("Chart.yaml 파싱 실패: %w", err)
	}

	fmt.Printf("  [로더] API 버전 감지: %s (차트: %s)\n", metadata.APIVersion, metadata.Name)

	// 3) Chart 구조체 구성
	chart := &Chart{
		Metadata: metadata,
		Values:   make(map[string]any),
	}

	// 4) values.yaml 로드
	valuesPath := filepath.Join(topdir, "values.yaml")
	if valData, err := os.ReadFile(valuesPath); err == nil {
		vals, _ := parseSimpleYaml(string(valData))
		chart.Values = vals
	}

	// 5) templates/ 디렉토리 로드
	templatesDir := filepath.Join(topdir, "templates")
	if entries, err := os.ReadDir(templatesDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			fdata, err := os.ReadFile(filepath.Join(templatesDir, entry.Name()))
			if err != nil {
				continue
			}
			chart.Templates = append(chart.Templates, &File{
				Name: "templates/" + entry.Name(),
				Data: fdata,
			})
		}
	}

	// 6) charts/ 하위 디렉토리에서 서브차트 로드 (재귀)
	// 실제 Helm: v2load.loadDependencies
	chartsDir := filepath.Join(topdir, "charts")
	if entries, err := os.ReadDir(chartsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			subLoader := &DirLoader{path: filepath.Join(chartsDir, entry.Name())}
			subChart, err := subLoader.Load()
			if err != nil {
				fmt.Printf("  [로더] 서브차트 로드 실패: %s: %v\n", entry.Name(), err)
				continue
			}
			chart.AddDependency(subChart)
		}
	}

	return chart, nil
}

func (l *FileLoader) Load() (*Chart, error) {
	return nil, fmt.Errorf("아카이브 로더는 이 PoC에서 지원하지 않습니다 (시뮬레이션만)")
}

// =============================================================================
// Chart.yaml 파서 (간소화 JSON 기반)
// 실제 Helm: sigs.k8s.io/yaml 사용 (YAML→JSON→struct)
// =============================================================================

func parseChartYaml(data []byte) (*Metadata, error) {
	// 간단한 YAML 파서 시뮬레이션 (key: value 형식만 지원)
	md := &Metadata{}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "apiVersion":
			md.APIVersion = val
		case "name":
			md.Name = val
		case "version":
			md.Version = val
		case "description":
			md.Description = val
		case "type":
			md.Type = val
		case "appVersion":
			md.AppVersion = val
		}
	}
	return md, nil
}

func parseSimpleYaml(data string) (map[string]any, error) {
	result := make(map[string]any)
	lines := strings.Split(data, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		result[key] = val
	}
	return result, nil
}

// =============================================================================
// .helmignore 패턴 매칭
// 실제 Helm: pkg/ignore/rules.go
// .gitignore 스타일 패턴으로 차트 로드 시 파일 제외
// =============================================================================

// HelmIgnore는 무시할 파일 패턴 목록이다.
type HelmIgnore struct {
	patterns []string
}

// NewHelmIgnore는 기본 패턴으로 HelmIgnore를 생성한다.
func NewHelmIgnore() *HelmIgnore {
	return &HelmIgnore{
		patterns: []string{
			".git",
			".gitignore",
			".helmignore",
			"*.swp",
			"*.bak",
			"*.tmp",
			".DS_Store",
		},
	}
}

// AddPattern은 무시 패턴을 추가한다.
func (hi *HelmIgnore) AddPattern(pattern string) {
	hi.patterns = append(hi.patterns, pattern)
}

// Ignore는 파일명이 무시 패턴에 매칭되는지 확인한다.
func (hi *HelmIgnore) Ignore(name string) bool {
	base := filepath.Base(name)
	for _, pattern := range hi.patterns {
		// 단순 매칭 (실제 Helm은 filepath.Match + 디렉토리 패턴 지원)
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
		if base == pattern {
			return true
		}
	}
	return false
}

// =============================================================================
// 시뮬레이션용 차트 디렉토리 생성
// =============================================================================

func createSimulatedChartDir(baseDir string) error {
	// 루트 차트
	rootDir := filepath.Join(baseDir, "myapp")
	dirs := []string{
		rootDir,
		filepath.Join(rootDir, "templates"),
		filepath.Join(rootDir, "charts"),
		filepath.Join(rootDir, "charts", "redis"),
		filepath.Join(rootDir, "charts", "redis", "templates"),
		filepath.Join(rootDir, "charts", "postgresql"),
		filepath.Join(rootDir, "charts", "postgresql", "templates"),
	}

	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}

	// Chart.yaml
	files := map[string]string{
		filepath.Join(rootDir, "Chart.yaml"): `apiVersion: v2
name: myapp
version: 1.0.0
description: My Application
type: application
appVersion: 2.0.0`,

		filepath.Join(rootDir, "values.yaml"): `replicaCount: 3
image: nginx:1.21`,

		filepath.Join(rootDir, "templates", "deployment.yaml"): `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Values.name }}`,

		filepath.Join(rootDir, "templates", "_helpers.tpl"): `{{- define "myapp.name" -}}
myapp
{{- end -}}`,

		// Redis 서브차트
		filepath.Join(rootDir, "charts", "redis", "Chart.yaml"): `apiVersion: v2
name: redis
version: 17.3.14
description: Redis dependency
type: application`,

		filepath.Join(rootDir, "charts", "redis", "values.yaml"): `architecture: standalone
replicas: 1`,

		filepath.Join(rootDir, "charts", "redis", "templates", "statefulset.yaml"): `kind: StatefulSet
metadata:
  name: redis`,

		// PostgreSQL 서브차트
		filepath.Join(rootDir, "charts", "postgresql", "Chart.yaml"): `apiVersion: v2
name: postgresql
version: 12.1.9
description: PostgreSQL dependency
type: application`,

		filepath.Join(rootDir, "charts", "postgresql", "values.yaml"): `auth: enabled`,

		filepath.Join(rootDir, "charts", "postgresql", "templates", "statefulset.yaml"): `kind: StatefulSet
metadata:
  name: postgresql`,
	}

	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return err
		}
	}

	return nil
}

// =============================================================================
// main: 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== Helm v4 Chart 로더 PoC ===")
	fmt.Println()

	// 1) 시뮬레이션 차트 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "helm-loader-poc")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	if err := createSimulatedChartDir(tmpDir); err != nil {
		fmt.Printf("차트 디렉토리 생성 실패: %v\n", err)
		return
	}

	chartDir := filepath.Join(tmpDir, "myapp")
	fmt.Printf("시뮬레이션 차트 경로: %s\n\n", chartDir)

	// 2) 로더 감지
	demoLoaderDetection(chartDir)

	// 3) 디렉토리에서 차트 로드
	chart := demoLoadChart(chartDir)
	if chart == nil {
		return
	}

	// 4) 로드된 차트 구조 출력
	demoChartStructure(chart)

	// 5) .helmignore 패턴 매칭
	demoHelmIgnore()

	// 6) Chart.yaml 유효성 검사
	demoValidation()

	fmt.Println("=== Chart 로더 PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. Loader(): os.Stat → 디렉토리/파일 구분 → DirLoader 또는 FileLoader")
	fmt.Println("  2. API 버전 감지: Chart.yaml의 apiVersion → v1/v2/v3 로더 선택")
	fmt.Println("  3. 디렉토리 로드: Chart.yaml → values.yaml → templates/ → charts/ (재귀)")
	fmt.Println("  4. 의존성 트리: charts/ 하위 디렉토리를 서브차트로 로드하여 parent-child 관계 설정")
	fmt.Println("  5. .helmignore: .gitignore 스타일 패턴으로 불필요한 파일 제외")
}

func demoLoaderDetection(chartDir string) {
	fmt.Println("--- 1. 로더 타입 감지 ---")

	l, err := Loader(chartDir)
	if err != nil {
		fmt.Printf("  에러: %v\n", err)
		return
	}

	switch l.(type) {
	case *DirLoader:
		fmt.Printf("  %s → DirLoader 선택됨\n", chartDir)
	case *FileLoader:
		fmt.Printf("  %s → FileLoader 선택됨\n", chartDir)
	}

	// 존재하지 않는 경로
	_, err = Loader("/nonexistent/path")
	fmt.Printf("  /nonexistent/path → 에러: %v\n", err)

	fmt.Println()
}

func demoLoadChart(chartDir string) *Chart {
	fmt.Println("--- 2. 차트 로드 (디렉토리 → Chart 구조체) ---")

	chart, err := Load(chartDir)
	if err != nil {
		fmt.Printf("  로드 실패: %v\n", err)
		return nil
	}

	fmt.Printf("  로드 성공: %s v%s\n", chart.Name(), chart.Metadata.Version)
	fmt.Println()
	return chart
}

func demoChartStructure(chart *Chart) {
	fmt.Println("--- 3. 로드된 차트 구조 ---")

	printChartTree(chart, "  ")
	fmt.Println()

	// 상세 정보
	fmt.Println("  [루트 차트 상세]")
	data, _ := json.MarshalIndent(chart.Metadata, "    ", "  ")
	fmt.Printf("    Metadata: %s\n", string(data))
	fmt.Printf("    Values: %v\n", chart.Values)
	fmt.Printf("    Templates: %d개\n", len(chart.Templates))
	for _, t := range chart.Templates {
		fmt.Printf("      - %s (%d bytes)\n", t.Name, len(t.Data))
	}

	fmt.Println()
	for _, dep := range chart.Dependencies() {
		fmt.Printf("  [서브차트: %s]\n", dep.Name())
		fmt.Printf("    Version: %s\n", dep.Metadata.Version)
		fmt.Printf("    Values: %v\n", dep.Values)
		fmt.Printf("    Templates: %d개\n", len(dep.Templates))
	}

	fmt.Println()
}

func printChartTree(ch *Chart, indent string) {
	chartType := ch.Metadata.Type
	if chartType == "" {
		chartType = "application"
	}
	fmt.Printf("%s%s v%s [%s]\n", indent, ch.Name(), ch.Metadata.Version, chartType)
	for _, dep := range ch.Dependencies() {
		printChartTree(dep, indent+"  ")
	}
}

func demoHelmIgnore() {
	fmt.Println("--- 4. .helmignore 패턴 매칭 ---")

	ignore := NewHelmIgnore()
	ignore.AddPattern("*.test.yaml")

	testFiles := []string{
		"Chart.yaml",
		"values.yaml",
		".git",
		".gitignore",
		".DS_Store",
		"templates/deployment.yaml",
		"backup.bak",
		"temp.swp",
		"connection.test.yaml",
		"README.md",
	}

	for _, f := range testFiles {
		ignored := ignore.Ignore(f)
		status := "포함"
		if ignored {
			status = "제외"
		}
		fmt.Printf("  %-30s → %s\n", f, status)
	}

	fmt.Println()
}

func demoValidation() {
	fmt.Println("--- 5. Chart.yaml 유효성 검사 ---")

	tests := []struct {
		name     string
		metadata Metadata
	}{
		{
			name:     "유효한 차트",
			metadata: Metadata{APIVersion: "v2", Name: "myapp", Version: "1.0.0"},
		},
		{
			name:     "apiVersion 누락",
			metadata: Metadata{Name: "myapp", Version: "1.0.0"},
		},
		{
			name:     "name 누락",
			metadata: Metadata{APIVersion: "v2", Version: "1.0.0"},
		},
		{
			name:     "version 누락",
			metadata: Metadata{APIVersion: "v2", Name: "myapp"},
		},
	}

	for _, tt := range tests {
		err := tt.metadata.Validate()
		if err != nil {
			fmt.Printf("  %-20s → 실패: %v\n", tt.name, err)
		} else {
			fmt.Printf("  %-20s → 통과\n", tt.name)
		}
	}

	fmt.Println()
}
