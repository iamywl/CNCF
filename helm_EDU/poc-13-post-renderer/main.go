package main

import (
	"bytes"
	"fmt"
	"strings"
)

// =============================================================================
// Helm PostRenderer PoC
// =============================================================================
//
// 참조: pkg/postrenderer/postrenderer.go, pkg/action/action.go (annotateAndMerge)
//
// PostRenderer는 Helm이 매니페스트를 렌더링한 후, Kubernetes에 적용하기 전에
// 매니페스트를 변환할 수 있는 인터페이스이다.
// 이 PoC는 다음을 시뮬레이션한다:
//   1. PostRenderer 인터페이스 — 매니페스트 변환 파이프라인
//   2. 파이프라인 — 여러 PostRenderer를 체이닝
//   3. 매니페스트 변환 — 주석/라벨 추가, 리소스 수정
//   4. annotateAndMerge / splitAndDeannotate — 멀티 문서 YAML 처리
// =============================================================================

// --- PostRenderer 인터페이스 ---
// Helm 소스: pkg/postrenderer/postrenderer.go의 PostRenderer
// Run은 렌더링된 매니페스트 버퍼를 받아 수정된 버퍼를 반환한다.
type PostRenderer interface {
	Run(renderedManifests *bytes.Buffer) (modifiedManifests *bytes.Buffer, err error)
}

// --- LabelInjector: 라벨 추가 PostRenderer ---
// 모든 리소스에 지정된 라벨을 추가한다.
type LabelInjector struct {
	Labels map[string]string
}

func (li *LabelInjector) Run(in *bytes.Buffer) (*bytes.Buffer, error) {
	docs := splitYAMLDocuments(in.String())
	var result []string

	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		modified := injectLabels(doc, li.Labels)
		result = append(result, modified)
	}

	out := bytes.NewBufferString(strings.Join(result, "\n---\n"))
	return out, nil
}

// --- AnnotationInjector: 주석 추가 PostRenderer ---
type AnnotationInjector struct {
	Annotations map[string]string
}

func (ai *AnnotationInjector) Run(in *bytes.Buffer) (*bytes.Buffer, error) {
	docs := splitYAMLDocuments(in.String())
	var result []string

	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		modified := injectAnnotations(doc, ai.Annotations)
		result = append(result, modified)
	}

	out := bytes.NewBufferString(strings.Join(result, "\n---\n"))
	return out, nil
}

// --- NamespaceOverride: 네임스페이스 강제 변경 PostRenderer ---
type NamespaceOverride struct {
	Namespace string
}

func (no *NamespaceOverride) Run(in *bytes.Buffer) (*bytes.Buffer, error) {
	docs := splitYAMLDocuments(in.String())
	var result []string

	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		modified := overrideNamespace(doc, no.Namespace)
		result = append(result, modified)
	}

	out := bytes.NewBufferString(strings.Join(result, "\n---\n"))
	return out, nil
}

// --- ImageRewriter: 이미지 레지스트리 변경 PostRenderer ---
type ImageRewriter struct {
	OldRegistry string
	NewRegistry string
}

func (ir *ImageRewriter) Run(in *bytes.Buffer) (*bytes.Buffer, error) {
	content := in.String()
	// 간단한 문자열 치환으로 이미지 레지스트리 변경
	modified := strings.ReplaceAll(content, ir.OldRegistry, ir.NewRegistry)
	return bytes.NewBufferString(modified), nil
}

// --- PostRendererChain: 여러 PostRenderer를 순차 실행하는 파이프라인 ---
// Helm에서는 하나의 PostRenderer만 지정하지만,
// 실제 사용 시 체이닝 패턴이 일반적이다.
type PostRendererChain struct {
	Renderers []PostRenderer
}

func (c *PostRendererChain) Run(in *bytes.Buffer) (*bytes.Buffer, error) {
	current := in
	for i, r := range c.Renderers {
		result, err := r.Run(current)
		if err != nil {
			return nil, fmt.Errorf("PostRenderer[%d] 실행 실패: %w", i, err)
		}
		// 빈 출력 검사 (실제 Helm에서도 수행)
		if len(bytes.TrimSpace(result.Bytes())) == 0 {
			return nil, fmt.Errorf("PostRenderer[%d] 출력이 비어 있음", i)
		}
		current = result
	}
	return current, nil
}

// --- annotateAndMerge / splitAndDeannotate ---
// Helm 소스: pkg/action/action.go의 annotateAndMerge
//
// 멀티 문서 YAML에서 각 리소스를 고유하게 식별하기 위해
// 내부 주석(helm.sh/resource-policy 등)을 추가한 후 병합한다.
// 이후 splitAndDeannotate로 주석을 제거하고 다시 분리한다.

// annotateAndMerge는 여러 파일의 매니페스트를 하나의 문서로 병합한다.
// Helm 소스: pkg/action/action.go
// 각 리소스에 원본 파일 경로 주석을 추가하여 추적 가능하게 한다.
func annotateAndMerge(files map[string]string) string {
	var parts []string
	for path, content := range files {
		docs := splitYAMLDocuments(content)
		for _, doc := range docs {
			doc = strings.TrimSpace(doc)
			if doc == "" || doc == "---" {
				continue
			}
			// 원본 경로 주석 추가
			annotated := fmt.Sprintf("# Source: %s\n%s", path, doc)
			parts = append(parts, annotated)
		}
	}
	return strings.Join(parts, "\n---\n")
}

// splitAndDeannotate는 병합된 문서를 다시 분리하고 주석을 제거한다.
func splitAndDeannotate(merged string) map[string]string {
	result := make(map[string]string)
	docs := splitYAMLDocuments(merged)

	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		// Source 주석에서 경로 추출
		lines := strings.SplitN(doc, "\n", 2)
		if len(lines) < 2 {
			continue
		}

		if strings.HasPrefix(lines[0], "# Source: ") {
			path := strings.TrimPrefix(lines[0], "# Source: ")
			content := lines[1]
			if existing, ok := result[path]; ok {
				result[path] = existing + "\n---\n" + content
			} else {
				result[path] = content
			}
		}
	}

	return result
}

// --- YAML 유틸리티 ---

// splitYAMLDocuments는 "---"로 YAML 문서를 분리한다.
func splitYAMLDocuments(s string) []string {
	return strings.Split(s, "\n---\n")
}

// injectLabels는 YAML 문서에 라벨을 추가한다 (간이 구현).
func injectLabels(doc string, labels map[string]string) string {
	// metadata: 섹션 찾기
	if !strings.Contains(doc, "metadata:") {
		return doc
	}

	// labels: 섹션이 있는지 확인
	if strings.Contains(doc, "  labels:") {
		// 기존 labels에 추가
		var labelsStr string
		for k, v := range labels {
			labelsStr += fmt.Sprintf("    %s: %s\n", k, v)
		}
		doc = strings.Replace(doc, "  labels:\n", "  labels:\n"+labelsStr, 1)
	} else {
		// labels 섹션 생성
		var labelsStr string
		for k, v := range labels {
			labelsStr += fmt.Sprintf("    %s: %s\n", k, v)
		}
		doc = strings.Replace(doc, "metadata:\n", "metadata:\n  labels:\n"+labelsStr, 1)
	}

	return doc
}

// injectAnnotations는 YAML 문서에 주석을 추가한다.
func injectAnnotations(doc string, annotations map[string]string) string {
	if !strings.Contains(doc, "metadata:") {
		return doc
	}

	if strings.Contains(doc, "  annotations:") {
		var annoStr string
		for k, v := range annotations {
			annoStr += fmt.Sprintf("    %s: %s\n", k, v)
		}
		doc = strings.Replace(doc, "  annotations:\n", "  annotations:\n"+annoStr, 1)
	} else {
		var annoStr string
		for k, v := range annotations {
			annoStr += fmt.Sprintf("    %s: %s\n", k, v)
		}
		doc = strings.Replace(doc, "metadata:\n", "metadata:\n  annotations:\n"+annoStr, 1)
	}

	return doc
}

// overrideNamespace는 네임스페이스를 강제 변경한다.
func overrideNamespace(doc string, namespace string) string {
	if strings.Contains(doc, "  namespace:") {
		lines := strings.Split(doc, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "namespace:") &&
				strings.Contains(doc[:strings.Index(doc, line)], "metadata:") {
				lines[i] = "  namespace: " + namespace
			}
		}
		return strings.Join(lines, "\n")
	}
	// namespace가 없으면 metadata 뒤에 추가
	return strings.Replace(doc, "metadata:\n", "metadata:\n  namespace: "+namespace+"\n", 1)
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              Helm PostRenderer PoC                           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("참조: pkg/postrenderer/postrenderer.go, pkg/action/action.go")
	fmt.Println()

	// 샘플 렌더링된 매니페스트
	renderedManifest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  labels:
    app: my-app
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: my-app
        image: docker.io/myorg/my-app:v1.0
---
apiVersion: v1
kind: Service
metadata:
  name: my-app-svc
spec:
  type: ClusterIP
  ports:
  - port: 80`

	// =================================================================
	// 1. PostRenderer 인터페이스 소개
	// =================================================================
	fmt.Println("1. PostRenderer 인터페이스")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  PostRenderer 인터페이스:
  ┌──────────────────────────────────────────┐
  │  type PostRenderer interface {            │
  │    Run(in *bytes.Buffer)                  │
  │        (*bytes.Buffer, error)             │
  │  }                                        │
  └──────────────────────────────────────────┘

  사용 사례:
  - kustomize로 매니페스트 후처리
  - istio sidecar injection
  - 라벨/주석 일괄 추가
  - 이미지 레지스트리 변경
  - 네임스페이스 강제 지정
`)

	fmt.Println("  원본 매니페스트:")
	for _, line := range strings.Split(renderedManifest, "\n") {
		fmt.Printf("    %s\n", line)
	}

	// =================================================================
	// 2. 라벨 주입 PostRenderer
	// =================================================================
	fmt.Println("\n2. LabelInjector — 라벨 추가")
	fmt.Println(strings.Repeat("-", 60))

	labelInjector := &LabelInjector{
		Labels: map[string]string{
			"team":        "platform",
			"environment": "production",
		},
	}

	result, err := labelInjector.Run(bytes.NewBufferString(renderedManifest))
	if err != nil {
		fmt.Printf("오류: %v\n", err)
		return
	}

	fmt.Println("  라벨 추가 결과:")
	for _, line := range strings.Split(result.String(), "\n") {
		fmt.Printf("    %s\n", line)
	}

	// =================================================================
	// 3. PostRenderer 체이닝
	// =================================================================
	fmt.Println("\n3. PostRenderer 체이닝 (파이프라인)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  여러 PostRenderer를 순차 적용:")
	fmt.Println("  LabelInjector → AnnotationInjector → ImageRewriter → NamespaceOverride")

	chain := &PostRendererChain{
		Renderers: []PostRenderer{
			&LabelInjector{Labels: map[string]string{
				"managed-by": "helm",
				"tier":       "backend",
			}},
			&AnnotationInjector{Annotations: map[string]string{
				"prometheus.io/scrape": "true",
			}},
			&ImageRewriter{
				OldRegistry: "docker.io/myorg",
				NewRegistry: "registry.internal.com/myorg",
			},
			&NamespaceOverride{
				Namespace: "production",
			},
		},
	}

	chainResult, err := chain.Run(bytes.NewBufferString(renderedManifest))
	if err != nil {
		fmt.Printf("오류: %v\n", err)
		return
	}

	fmt.Println("\n  체이닝 결과:")
	for _, line := range strings.Split(chainResult.String(), "\n") {
		fmt.Printf("    %s\n", line)
	}

	// =================================================================
	// 4. annotateAndMerge / splitAndDeannotate
	// =================================================================
	fmt.Println("\n4. annotateAndMerge / splitAndDeannotate")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  Helm은 여러 템플릿 파일의 출력을 하나의 문서로 병합할 때")
	fmt.Println("  각 리소스의 원본 경로를 '# Source:' 주석으로 추적한다.")

	templateFiles := map[string]string{
		"templates/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 2`,
		"templates/service.yaml": `apiVersion: v1
kind: Service
metadata:
  name: web-svc
spec:
  type: ClusterIP`,
		"templates/configmap.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: web-config
data:
  key: value`,
	}

	fmt.Println("\n  원본 템플릿 파일:")
	for path, content := range templateFiles {
		fmt.Printf("    [%s]\n", path)
		for _, line := range strings.Split(content, "\n") {
			fmt.Printf("      %s\n", line)
		}
		fmt.Println()
	}

	// annotateAndMerge
	merged := annotateAndMerge(templateFiles)
	fmt.Println("  annotateAndMerge 결과 (하나의 문서로 병합):")
	for _, line := range strings.Split(merged, "\n") {
		fmt.Printf("    %s\n", line)
	}

	// PostRenderer 적용
	postRendered, _ := (&LabelInjector{Labels: map[string]string{
		"app.kubernetes.io/managed-by": "Helm",
	}}).Run(bytes.NewBufferString(merged))

	// splitAndDeannotate
	separated := splitAndDeannotate(postRendered.String())
	fmt.Println("\n  splitAndDeannotate 결과 (다시 파일별 분리):")
	for path, content := range separated {
		fmt.Printf("    [%s]\n", path)
		for _, line := range strings.Split(content, "\n") {
			fmt.Printf("      %s\n", line)
		}
		fmt.Println()
	}

	// =================================================================
	// 5. 빈 출력 검사
	// =================================================================
	fmt.Println("5. 빈 출력 검사")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  PostRenderer가 거의 빈 출력을 반환하면 오류로 처리한다.")
	fmt.Println("  Helm 소스: postrenderer.go — 'produced empty output' 에러")

	emptyRenderer := &PostRendererChain{
		Renderers: []PostRenderer{
			PostRendererFunc(func(in *bytes.Buffer) (*bytes.Buffer, error) {
				return bytes.NewBufferString("  \n  \n"), nil // 거의 빈 출력
			}),
		},
	}

	_, err = emptyRenderer.Run(bytes.NewBufferString(renderedManifest))
	if err != nil {
		fmt.Printf("  오류 (예상): %v\n", err)
	}

	// =================================================================
	// 6. 아키텍처 다이어그램
	// =================================================================
	fmt.Println("\n6. PostRenderer 아키텍처")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  helm install --post-renderer ./kustomize-wrapper
       │
       v
  ┌─────────────────────────────────────────┐
  │  1. 템플릿 렌더링 (Go template)          │
  │     templates/*.yaml → 렌더링된 매니페스트  │
  └────────┬────────────────────────────────┘
           │
           v
  ┌─────────────────────────────────────────┐
  │  2. annotateAndMerge                     │
  │     여러 파일 → # Source: 주석 추가        │
  │     하나의 문서로 병합                     │
  └────────┬────────────────────────────────┘
           │
           v
  ┌─────────────────────────────────────────┐
  │  3. PostRenderer.Run(buffer)             │
  │     ┌───────────┐  ┌───────────┐        │
  │     │ stdin      │→│ 외부 명령  │        │
  │     │ (manifests)│  │ (kustomize│        │
  │     │            │←│  /envsubst)│        │
  │     │ stdout     │  └───────────┘        │
  │     └───────────┘                        │
  │     또는 내장 PostRenderer:               │
  │     - LabelInjector                      │
  │     - AnnotationInjector                 │
  │     - ImageRewriter                      │
  │     - Plugin (postrenderer/v1 타입)      │
  └────────┬────────────────────────────────┘
           │
           v
  ┌─────────────────────────────────────────┐
  │  4. splitAndDeannotate                   │
  │     # Source: 주석으로 파일별 분리          │
  │     주석 제거                              │
  └────────┬────────────────────────────────┘
           │
           v
  ┌─────────────────────────────────────────┐
  │  5. KubeClient.Create / Update           │
  │     수정된 매니페스트를 K8s에 적용          │
  └─────────────────────────────────────────┘

  Helm v4 Plugin PostRenderer:
  ┌─────────────────────────────────────────┐
  │  plugin.yaml:                            │
  │    apiVersion: v1                        │
  │    type: postrenderer/v1                 │
  │    runtime: subprocess                   │
  │                                          │
  │  Plugin.Invoke(Input{                    │
  │    Message: {Manifests: buffer}           │
  │  }) → Output{Manifests: modified}        │
  └─────────────────────────────────────────┘
`)
}

// PostRendererFunc는 함수를 PostRenderer로 감싸는 어댑터이다.
type PostRendererFunc func(*bytes.Buffer) (*bytes.Buffer, error)

func (f PostRendererFunc) Run(in *bytes.Buffer) (*bytes.Buffer, error) {
	return f(in)
}
