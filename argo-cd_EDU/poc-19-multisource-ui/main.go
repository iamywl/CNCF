// Package main은 Argo CD의 Multi-Source Applications와 UI 서브시스템을
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Multi-Source 데이터 모델 (Sources[] vs 단일 Source)
// 2. Ref 소스 (다른 소스에서 값 참조)
// 3. 소스별 매니페스트 생성 및 병합
// 4. 하위 호환성 (Source ↔ Sources 변환)
// 5. Helm Values 파일 참조 ($ref)
// 6. UI 컴포넌트 트리 구조 (React SPA 시뮬레이션)
// 7. 앱 상태 시각화 (리소스 트리)
// 8. UI 라우팅 패턴
// 9. 설정 페이지 구조
// 10. 앱 목록 페이지 필터/정렬
//
// 실제 소스 참조:
//   - pkg/apis/application/v1alpha1/types.go (ApplicationSource, Sources)
//   - reposerver/repository/repository.go     (다중 소스 매니페스트 생성)
//   - ui/src/app/                             (React SPA)
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ============================================================================
// 1. Multi-Source 데이터 모델
//    (pkg/apis/application/v1alpha1/types.go 시뮬레이션)
// ============================================================================

// ApplicationSource는 단일 소스를 나타낸다.
type ApplicationSource struct {
	RepoURL        string                `json:"repoURL"`
	Path           string                `json:"path,omitempty"`
	TargetRevision string                `json:"targetRevision,omitempty"`
	Helm           *ApplicationSourceHelm `json:"helm,omitempty"`
	Kustomize      *ApplicationSourceKustomize `json:"kustomize,omitempty"`
	Chart          string                `json:"chart,omitempty"`
	Ref            string                `json:"ref,omitempty"`  // 참조 이름
	Name           string                `json:"name,omitempty"` // 소스 이름
}

// ApplicationSourceHelm은 Helm 관련 설정이다.
type ApplicationSourceHelm struct {
	ValueFiles  []string `json:"valueFiles,omitempty"`
	Parameters  []HelmParameter `json:"parameters,omitempty"`
	ReleaseName string   `json:"releaseName,omitempty"`
}

// HelmParameter는 Helm 파라미터다.
type HelmParameter struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ApplicationSourceKustomize는 Kustomize 관련 설정이다.
type ApplicationSourceKustomize struct {
	NamePrefix string `json:"namePrefix,omitempty"`
	NameSuffix string `json:"nameSuffix,omitempty"`
}

// ApplicationSpec은 앱 스펙이다. Source와 Sources는 상호 배타적이다.
type ApplicationSpec struct {
	Source      *ApplicationSource   `json:"source,omitempty"`
	Sources     []ApplicationSource  `json:"sources,omitempty"`
	Destination Destination          `json:"destination"`
	Project     string               `json:"project"`
}

// Destination은 배포 대상이다.
type Destination struct {
	Server    string `json:"server"`
	Namespace string `json:"namespace"`
}

// GetSources는 Source 또는 Sources를 통합하여 반환한다 (하위 호환성).
// 실제 구현: types.go의 ApplicationSpec.GetSources()
func (spec *ApplicationSpec) GetSources() []ApplicationSource {
	if len(spec.Sources) > 0 {
		return spec.Sources
	}
	if spec.Source != nil {
		return []ApplicationSource{*spec.Source}
	}
	return nil
}

// IsMultiSource는 다중 소스 앱인지 확인한다.
func (spec *ApplicationSpec) IsMultiSource() bool {
	return len(spec.Sources) > 0
}

// ============================================================================
// 2. Ref 소스 해석 (다른 소스에서 값 참조)
// ============================================================================

// ResolveRef는 $ref/path 형태의 값 파일 참조를 해석한다.
// 예: $values/charts/common/values.yaml → "values"라는 Ref를 가진 소스의 해당 경로
func ResolveRef(valueFile string, sources []ApplicationSource) (string, string, error) {
	if !strings.HasPrefix(valueFile, "$") {
		return "", valueFile, nil // 일반 파일 경로
	}

	// $refName/path 파싱
	parts := strings.SplitN(valueFile[1:], "/", 2)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("잘못된 Ref 형식: %s", valueFile)
	}

	refName := parts[0]
	path := parts[1]

	// Ref 소스 찾기
	for _, src := range sources {
		if src.Ref == refName {
			return src.RepoURL, path, nil
		}
	}

	return "", "", fmt.Errorf("Ref %q를 가진 소스를 찾을 수 없음", refName)
}

// ============================================================================
// 3. 매니페스트 생성 시뮬레이션
// ============================================================================

// Manifest는 생성된 Kubernetes 매니페스트다.
type Manifest struct {
	SourceIndex int    `json:"sourceIndex"`
	SourceName  string `json:"sourceName,omitempty"`
	Content     string `json:"content"`
}

// GenerateManifests는 다중 소스의 매니페스트를 생성한다.
func GenerateManifests(spec *ApplicationSpec) []Manifest {
	sources := spec.GetSources()
	var manifests []Manifest

	for i, src := range sources {
		// Ref 소스는 매니페스트를 생성하지 않음 (값 참조용)
		if src.Ref != "" {
			fmt.Printf("  소스 %d [%s]: Ref 소스 — 매니페스트 생성 건너뜀\n", i, src.Ref)
			continue
		}

		var content string
		if src.Chart != "" {
			// Helm 차트
			content = generateHelmManifest(src, sources)
		} else if src.Kustomize != nil {
			// Kustomize
			content = generateKustomizeManifest(src)
		} else {
			// Plain YAML
			content = generatePlainManifest(src)
		}

		manifests = append(manifests, Manifest{
			SourceIndex: i,
			SourceName:  src.Name,
			Content:     content,
		})
	}

	return manifests
}

func generateHelmManifest(src ApplicationSource, allSources []ApplicationSource) string {
	var lines []string
	lines = append(lines, "# Helm chart: "+src.Chart)
	lines = append(lines, "# Repo: "+src.RepoURL)

	if src.Helm != nil {
		for _, vf := range src.Helm.ValueFiles {
			repo, path, err := ResolveRef(vf, allSources)
			if err != nil {
				lines = append(lines, fmt.Sprintf("# ERROR: %v", err))
			} else if repo != "" {
				lines = append(lines, fmt.Sprintf("# Values from Ref: %s → %s", repo, path))
			} else {
				lines = append(lines, fmt.Sprintf("# Values: %s", path))
			}
		}
		for _, p := range src.Helm.Parameters {
			lines = append(lines, fmt.Sprintf("# Param: %s=%s", p.Name, p.Value))
		}
	}

	lines = append(lines, "---")
	lines = append(lines, "apiVersion: apps/v1")
	lines = append(lines, "kind: Deployment")
	lines = append(lines, fmt.Sprintf("metadata:\n  name: %s", src.Chart))

	return strings.Join(lines, "\n")
}

func generateKustomizeManifest(src ApplicationSource) string {
	return fmt.Sprintf("# Kustomize: %s/%s\n# Prefix: %s\n---\napiVersion: apps/v1\nkind: Deployment",
		src.RepoURL, src.Path, src.Kustomize.NamePrefix)
}

func generatePlainManifest(src ApplicationSource) string {
	return fmt.Sprintf("# Plain YAML: %s/%s\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: config",
		src.RepoURL, src.Path)
}

// ============================================================================
// 4. UI 시뮬레이션 (React SPA 구조)
// ============================================================================

// UIRoute는 UI 라우팅 패턴이다.
type UIRoute struct {
	Path        string
	Component   string
	Description string
}

// UIRoutes는 Argo CD UI의 라우팅 테이블이다.
var UIRoutes = []UIRoute{
	{"/applications", "ApplicationList", "앱 목록 페이지"},
	{"/applications/:name", "ApplicationDetails", "앱 상세 페이지"},
	{"/applications/:name/:tab", "ApplicationDetails", "앱 상세 탭 (summary, diff, events, logs)"},
	{"/settings", "Settings", "설정 페이지"},
	{"/settings/clusters", "ClusterList", "클러스터 관리"},
	{"/settings/repos", "RepoList", "레포지토리 관리"},
	{"/settings/projects", "ProjectList", "프로젝트 관리"},
	{"/login", "Login", "로그인 페이지"},
}

// ResourceNode는 리소스 트리의 노드다.
type ResourceNode struct {
	Kind      string
	Name      string
	Namespace string
	Health    string
	Children  []*ResourceNode
}

// PrintTree는 리소스 트리를 출력한다.
func PrintTree(node *ResourceNode, prefix string, isLast bool) {
	connector := "├── "
	if isLast {
		connector = "└── "
	}
	if prefix == "" {
		fmt.Printf("  %s/%s (%s)\n", node.Kind, node.Name, node.Health)
	} else {
		fmt.Printf("  %s%s%s/%s (%s)\n", prefix, connector, node.Kind, node.Name, node.Health)
	}

	childPrefix := prefix
	if isLast {
		childPrefix += "    "
	} else {
		childPrefix += "│   "
	}

	for i, child := range node.Children {
		PrintTree(child, childPrefix, i == len(node.Children)-1)
	}
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Argo CD Multi-Source Applications & UI 시뮬레이션 PoC      ║")
	fmt.Println("║  실제 소스: pkg/apis/application/, ui/src/app/              ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. 단일 소스 앱 (하위 호환) ===
	fmt.Println("=== 1. 단일 소스 앱 (하위 호환) ===")
	singleApp := ApplicationSpec{
		Source: &ApplicationSource{
			RepoURL:        "https://github.com/myorg/k8s-configs",
			Path:           "apps/web",
			TargetRevision: "main",
		},
		Destination: Destination{
			Server:    "https://kubernetes.default.svc",
			Namespace: "production",
		},
		Project: "default",
	}

	fmt.Printf("  IsMultiSource: %v\n", singleApp.IsMultiSource())
	fmt.Printf("  Sources 수: %d\n", len(singleApp.GetSources()))
	specJSON, _ := json.MarshalIndent(singleApp, "  ", "  ")
	fmt.Printf("  Spec:\n  %s\n\n", string(specJSON))

	// === 2. Multi-Source 앱 ===
	fmt.Println("=== 2. Multi-Source 앱 ===")
	multiApp := ApplicationSpec{
		Sources: []ApplicationSource{
			{
				// Ref 소스: values.yaml 제공
				RepoURL:        "https://github.com/myorg/helm-values",
				Path:           "production",
				TargetRevision: "main",
				Ref:            "values",
				Name:           "values-repo",
			},
			{
				// Helm 차트: Ref 소스의 values를 사용
				RepoURL:        "https://charts.bitnami.com/bitnami",
				Chart:          "nginx",
				TargetRevision: "15.0.0",
				Name:           "nginx-chart",
				Helm: &ApplicationSourceHelm{
					ValueFiles: []string{
						"$values/production/nginx-values.yaml",
						"$values/common/base-values.yaml",
					},
					Parameters: []HelmParameter{
						{Name: "replicaCount", Value: "3"},
						{Name: "service.type", Value: "LoadBalancer"},
					},
					ReleaseName: "web-nginx",
				},
			},
			{
				// Kustomize: 추가 매니페스트
				RepoURL:        "https://github.com/myorg/infra-configs",
				Path:           "overlays/production",
				TargetRevision: "main",
				Name:           "infra-kustomize",
				Kustomize: &ApplicationSourceKustomize{
					NamePrefix: "prod-",
				},
			},
		},
		Destination: Destination{
			Server:    "https://prod-cluster.example.com:6443",
			Namespace: "web",
		},
		Project: "web-team",
	}

	fmt.Printf("  IsMultiSource: %v\n", multiApp.IsMultiSource())
	fmt.Printf("  Sources 수: %d\n", len(multiApp.GetSources()))
	for i, src := range multiApp.Sources {
		typ := "Git (Plain)"
		if src.Chart != "" {
			typ = "Helm Chart"
		} else if src.Kustomize != nil {
			typ = "Kustomize"
		}
		if src.Ref != "" {
			typ = "Ref (" + src.Ref + ")"
		}
		fmt.Printf("  소스 %d: [%s] %s — %s @%s\n", i, typ, src.Name, src.RepoURL, src.TargetRevision)
	}
	fmt.Println()

	// === 3. Ref 해석 ===
	fmt.Println("=== 3. Ref 소스 값 참조 해석 ===")
	testRefs := []string{
		"$values/production/nginx-values.yaml",
		"$values/common/base-values.yaml",
		"inline-values.yaml",
		"$nonexistent/path.yaml",
	}
	for _, vf := range testRefs {
		repo, path, err := ResolveRef(vf, multiApp.Sources)
		if err != nil {
			fmt.Printf("  %s → 오류: %v\n", vf, err)
		} else if repo != "" {
			fmt.Printf("  %s → 리포=%s, 경로=%s\n", vf, repo, path)
		} else {
			fmt.Printf("  %s → 로컬 파일: %s\n", vf, path)
		}
	}
	fmt.Println()

	// === 4. 매니페스트 생성 ===
	fmt.Println("=== 4. 매니페스트 생성 ===")
	manifests := GenerateManifests(&multiApp)
	for _, m := range manifests {
		name := m.SourceName
		if name == "" {
			name = fmt.Sprintf("소스 %d", m.SourceIndex)
		}
		fmt.Printf("  [%s]:\n", name)
		for _, line := range strings.Split(m.Content, "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}

	// === 5. UI 라우팅 ===
	fmt.Println("=== 5. UI 라우팅 (React SPA) ===")
	for _, route := range UIRoutes {
		fmt.Printf("  %-40s → %-25s %s\n", route.Path, route.Component, route.Description)
	}
	fmt.Println()

	// === 6. 리소스 트리 ===
	fmt.Println("=== 6. 리소스 트리 (앱 상세 페이지) ===")
	tree := &ResourceNode{
		Kind: "Application", Name: "web-frontend", Health: "Healthy",
		Children: []*ResourceNode{
			{Kind: "Service", Name: "web-svc", Health: "Healthy", Namespace: "production"},
			{Kind: "Deployment", Name: "web-deploy", Health: "Healthy", Namespace: "production",
				Children: []*ResourceNode{
					{Kind: "ReplicaSet", Name: "web-deploy-abc123", Health: "Healthy",
						Children: []*ResourceNode{
							{Kind: "Pod", Name: "web-deploy-abc123-xyz1", Health: "Healthy"},
							{Kind: "Pod", Name: "web-deploy-abc123-xyz2", Health: "Healthy"},
							{Kind: "Pod", Name: "web-deploy-abc123-xyz3", Health: "Progressing"},
						},
					},
				},
			},
			{Kind: "ConfigMap", Name: "web-config", Health: "Healthy", Namespace: "production"},
			{Kind: "Ingress", Name: "web-ingress", Health: "Healthy", Namespace: "production"},
		},
	}
	PrintTree(tree, "", true)
}
