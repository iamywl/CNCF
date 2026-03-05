// Helm v4 템플릿 엔진 PoC: Go text/template + 커스텀 함수
//
// 이 PoC는 Helm v4의 템플릿 렌더링 엔진을 시뮬레이션합니다:
//   1. Go text/template 기반 렌더링 (pkg/engine/engine.go)
//   2. 커스텀 함수 맵 (pkg/engine/funcs.go) - toYaml, upper, default, include
//   3. 값 스코핑 - 차트별 Values 분리 (recAllTpls 패턴)
//   4. 파셜 템플릿(_helpers.tpl)과 include 함수
//   5. required 함수와 에러 핸들링
//
// 실행: go run main.go

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

// =============================================================================
// 데이터 모델 (간소화)
// =============================================================================

// Values는 차트 설정 값들의 컬렉션
type Values map[string]any

// File은 차트 내 파일
type File struct {
	Name string
	Data string
}

// Chart은 차트 구조체 (간소화)
type Chart struct {
	Name         string
	Templates    []*File
	Values       Values
	Dependencies []*Chart
}

// =============================================================================
// 커스텀 템플릿 함수: Helm의 pkg/engine/funcs.go
// Helm은 sprig 라이브러리 + 자체 함수(toYaml, fromYaml, include, tpl, required 등)을 사용.
// 여기서는 표준 라이브러리만으로 핵심 함수를 구현.
// =============================================================================

// toYAML은 값을 YAML 형식 문자열로 변환한다.
// 실제 Helm: engine/funcs.go → toYAML(v any) string
// yaml.Marshal 대신 간단한 재귀 구현으로 시뮬레이션.
func toYAML(v any) string {
	return toYAMLIndent(v, 0)
}

func toYAMLIndent(v any, indent int) string {
	prefix := strings.Repeat("  ", indent)
	switch val := v.(type) {
	case map[string]any:
		var lines []string
		for k, vv := range val {
			child := toYAMLIndent(vv, indent+1)
			if _, ok := vv.(map[string]any); ok {
				lines = append(lines, fmt.Sprintf("%s%s:\n%s", prefix, k, child))
			} else if _, ok := vv.([]any); ok {
				lines = append(lines, fmt.Sprintf("%s%s:\n%s", prefix, k, child))
			} else {
				lines = append(lines, fmt.Sprintf("%s%s: %s", prefix, k, child))
			}
		}
		return strings.Join(lines, "\n")
	case []any:
		var lines []string
		for _, item := range val {
			lines = append(lines, fmt.Sprintf("%s- %v", prefix, item))
		}
		return strings.Join(lines, "\n")
	case string:
		return val
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%v", val)
	}
}

// toJSON은 값을 JSON 문자열로 변환한다.
// 실제 Helm: engine/funcs.go → toJSON(v any) string
func toJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}

// indent는 각 줄 앞에 지정된 수만큼 공백을 추가한다.
// Helm의 sprig에서 가져온 함수.
func indent(spaces int, v string) string {
	pad := strings.Repeat(" ", spaces)
	lines := strings.Split(v, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = pad + line
		}
	}
	return strings.Join(lines, "\n")
}

// nindent는 줄바꿈 후 indent를 적용한다.
func nindent(spaces int, v string) string {
	return "\n" + indent(spaces, v)
}

// defaultVal은 값이 비어있으면 기본값을 반환한다.
// 실제 Helm: sprig의 default 함수
func defaultVal(defaultValue any, given any) any {
	if given == nil || given == "" || given == 0 || given == false {
		return defaultValue
	}
	return given
}

// upper는 문자열을 대문자로 변환한다.
func upper(s string) string {
	return strings.ToUpper(s)
}

// lower는 문자열을 소문자로 변환한다.
func lower(s string) string {
	return strings.ToLower(s)
}

// quote는 문자열을 따옴표로 감싼다.
func quote(s string) string {
	return fmt.Sprintf("%q", s)
}

// trimSuffix는 접미사를 제거한다.
func trimSuffix(suffix, s string) string {
	return strings.TrimSuffix(s, suffix)
}

// required는 값이 비어있으면 에러를 발생시킨다.
// 실제 Helm: engine.go → initFunMap에서 lintMode에 따라 동작 분기
func required(msg string, val any) (any, error) {
	if val == nil {
		return nil, fmt.Errorf(msg)
	}
	if s, ok := val.(string); ok && s == "" {
		return nil, fmt.Errorf(msg)
	}
	return val, nil
}

// =============================================================================
// Engine: Helm의 pkg/engine/engine.go
// text/template을 사용하여 차트 템플릿을 렌더링하는 핵심 엔진.
// =============================================================================

// Engine은 Helm 템플릿 렌더링 엔진이다.
// 실제 Helm: engine.Engine{Strict, LintMode, clientProvider, EnableDNS, CustomTemplateFuncs}
type Engine struct {
	Strict   bool
	LintMode bool
}

// Render는 차트의 모든 템플릿을 렌더링한다.
// 실제 Helm: (e Engine) Render(chrt, values) (map[string]string, error)
//
// 핵심 로직:
// 1) allTemplates(chart, values)로 모든 템플릿+값 수집
// 2) 빈 부모 template을 만들고 모든 템플릿을 Parse
// 3) 각 템플릿을 ExecuteTemplate으로 실행
// 4) 파셜(_로 시작)은 렌더링에서 제외 (include로만 사용)
func (e *Engine) Render(chart *Chart, values Values) (map[string]string, error) {
	// 1) 렌더링할 템플릿 수집 (실제 Helm: allTemplates → recAllTpls)
	tpls := e.collectTemplates(chart, values)

	// 2) 부모 템플릿 생성
	t := template.New("gotpl")
	if e.Strict {
		t.Option("missingkey=error")
	} else {
		t.Option("missingkey=zero")
	}

	// 3) 함수 맵 등록 (실제 Helm: initFunMap)
	includedNames := make(map[string]int)
	funcMap := template.FuncMap{
		"toYaml":     toYAML,
		"toJson":     toJSON,
		"indent":     indent,
		"nindent":    nindent,
		"default":    defaultVal,
		"upper":      upper,
		"lower":      lower,
		"quote":      quote,
		"trimSuffix": trimSuffix,
		"required":   required,
		// include는 다른 이름 있는 템플릿을 포함한다.
		// 실제 Helm: includeFun(t, includedNames) - 재귀 방지 포함
		"include": func(name string, data any) (string, error) {
			var buf strings.Builder
			if v, ok := includedNames[name]; ok {
				if v > 100 {
					return "", fmt.Errorf("include 재귀 한도 초과: %s", name)
				}
				includedNames[name]++
			} else {
				includedNames[name] = 1
			}
			err := t.ExecuteTemplate(&buf, name, data)
			includedNames[name]--
			return buf.String(), err
		},
	}
	t.Funcs(funcMap)

	// 4) 모든 템플릿 파싱
	for name, tplData := range tpls {
		if _, err := t.New(name).Parse(tplData.tpl); err != nil {
			return nil, fmt.Errorf("파싱 에러 (%s): %w", name, err)
		}
	}

	// 5) 렌더링 (파셜 제외)
	rendered := make(map[string]string)
	for name, tplData := range tpls {
		// 파셜 템플릿(_로 시작)은 직접 렌더링하지 않음
		parts := strings.Split(name, "/")
		baseName := parts[len(parts)-1]
		if strings.HasPrefix(baseName, "_") {
			continue
		}

		// Template 정보 주입 (실제 Helm: vals["Template"] = Values{"Name": filename, ...})
		tplData.vals["Template"] = map[string]any{"Name": name}

		var buf strings.Builder
		if err := t.ExecuteTemplate(&buf, name, tplData.vals); err != nil {
			return nil, fmt.Errorf("렌더링 에러 (%s): %w", name, err)
		}

		result := strings.ReplaceAll(buf.String(), "<no value>", "")
		rendered[name] = result
	}

	return rendered, nil
}

// renderable은 렌더링 가능한 단위를 표현한다.
// 실제 Helm: engine.renderable{tpl, vals, basePath}
type renderable struct {
	tpl  string
	vals Values
}

// collectTemplates는 차트와 서브차트의 모든 템플릿을 수집한다.
// 실제 Helm: allTemplates → recAllTpls
// 핵심: 값 스코핑 - 서브차트는 자신의 이름에 해당하는 Values 하위 섹션만 받는다.
func (e *Engine) collectTemplates(ch *Chart, parentVals Values) map[string]renderable {
	tpls := make(map[string]renderable)

	// 현재 차트의 Values 결정 (스코핑)
	chartVals := ch.Values
	if parentVals != nil {
		// 부모 Values에서 이 차트 이름에 해당하는 섹션 가져오기
		if subVals, ok := parentVals[ch.Name]; ok {
			if m, ok := subVals.(map[string]any); ok {
				// 차트 기본값과 사용자 값 병합
				merged := make(map[string]any)
				for k, v := range chartVals {
					merged[k] = v
				}
				for k, v := range m {
					merged[k] = v
				}
				chartVals = merged
			}
		}
	}

	// 렌더링 컨텍스트 구성 (실제 Helm: next = {"Chart":..., "Values":..., "Release":..., ...})
	renderVals := Values{
		"Chart":  map[string]any{"Name": ch.Name},
		"Values": chartVals,
	}

	// Release, Capabilities 정보 전달 (상위에서 내려옴)
	if rel, ok := parentVals["Release"]; ok {
		renderVals["Release"] = rel
	}
	if cap, ok := parentVals["Capabilities"]; ok {
		renderVals["Capabilities"] = cap
	}

	// 현재 차트의 템플릿 수집
	for _, tmpl := range ch.Templates {
		fullPath := ch.Name + "/" + tmpl.Name
		tpls[fullPath] = renderable{
			tpl:  tmpl.Data,
			vals: renderVals,
		}
	}

	// 서브차트 재귀 수집
	for _, dep := range ch.Dependencies {
		subTpls := e.collectTemplates(dep, chartVals)
		for k, v := range subTpls {
			tpls[k] = v
		}
	}

	return tpls
}

// =============================================================================
// main: 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== Helm v4 템플릿 엔진 PoC ===")
	fmt.Println()

	// 1) 기본 템플릿 렌더링
	demoBasicRendering()

	// 2) 커스텀 함수 시연
	demoCustomFunctions()

	// 3) include + 파셜 템플릿
	demoIncludePartials()

	// 4) 서브차트 값 스코핑
	demoSubchartScoping()

	// 5) required 함수와 에러 처리
	demoRequired()
}

func demoBasicRendering() {
	fmt.Println("--- 1. 기본 템플릿 렌더링 ---")

	chart := &Chart{
		Name: "myapp",
		Templates: []*File{
			{
				Name: "templates/deployment.yaml",
				Data: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Values.name }}
  labels:
    app: {{ .Values.name }}
    chart: {{ .Chart.Name }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels:
      app: {{ .Values.name }}
  template:
    spec:
      containers:
        - name: {{ .Values.name }}
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
          ports:
            - containerPort: {{ .Values.containerPort }}`,
			},
		},
		Values: Values{
			"name":         "nginx",
			"replicaCount": 3,
			"image": map[string]any{
				"repository": "nginx",
				"tag":        "1.21",
			},
			"containerPort": 80,
		},
	}

	engine := &Engine{}
	// Release/Capabilities 정보 제공
	topValues := Values{
		"Values": chart.Values,
		"Release": map[string]any{
			"Name":      "my-release",
			"Namespace": "default",
		},
		"Capabilities": map[string]any{
			"KubeVersion": "v1.28.0",
		},
	}

	result, err := engine.Render(chart, topValues)
	if err != nil {
		fmt.Printf("  렌더링 에러: %v\n", err)
		return
	}

	for name, content := range result {
		fmt.Printf("  --- %s ---\n%s\n\n", name, content)
	}
}

func demoCustomFunctions() {
	fmt.Println("--- 2. 커스텀 함수 시연 ---")

	chart := &Chart{
		Name: "functest",
		Templates: []*File{
			{
				Name: "templates/configmap.yaml",
				Data: `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ lower .Values.appName }}-config
data:
  APP_NAME: {{ upper .Values.appName | quote }}
  DEFAULT_VALUE: {{ default "fallback-value" .Values.missingKey }}
  IMAGE_TAG: {{ default "latest" .Values.imageTag }}
  LABELS: |
{{ toYaml .Values.labels | indent 4 }}
  CONFIG_JSON: {{ toJson .Values.config }}`,
			},
		},
		Values: Values{
			"appName":  "MyApplication",
			"imageTag": "v2.0",
			"labels": map[string]any{
				"app":     "myapp",
				"version": "2.0",
				"tier":    "frontend",
			},
			"config": map[string]any{
				"debug":   true,
				"timeout": 30,
			},
		},
	}

	engine := &Engine{}
	result, err := engine.Render(chart, Values{})
	if err != nil {
		fmt.Printf("  렌더링 에러: %v\n", err)
		return
	}

	for name, content := range result {
		fmt.Printf("  --- %s ---\n%s\n\n", name, content)
	}
}

func demoIncludePartials() {
	fmt.Println("--- 3. include + 파셜 템플릿 (_helpers.tpl) ---")

	chart := &Chart{
		Name: "webapp",
		Templates: []*File{
			{
				// 파셜 템플릿: _로 시작하면 직접 렌더링되지 않음
				Name: "templates/_helpers.tpl",
				Data: `{{- define "webapp.fullname" -}}
{{- default .Chart.Name .Values.fullnameOverride | lower -}}
{{- end -}}

{{- define "webapp.labels" -}}
app.kubernetes.io/name: {{ include "webapp.fullname" . }}
app.kubernetes.io/version: {{ .Values.appVersion }}
app.kubernetes.io/managed-by: Helm
{{- end -}}

{{- define "webapp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "webapp.fullname" . }}
{{- end -}}`,
			},
			{
				Name: "templates/deployment.yaml",
				Data: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "webapp.fullname" . }}
  labels:
{{ include "webapp.labels" . | indent 4 }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels:
{{ include "webapp.selectorLabels" . | indent 6 }}
  template:
    metadata:
      labels:
{{ include "webapp.selectorLabels" . | indent 8 }}
    spec:
      containers:
        - name: {{ include "webapp.fullname" . }}
          image: "{{ .Values.image }}"`,
			},
			{
				Name: "templates/service.yaml",
				Data: `apiVersion: v1
kind: Service
metadata:
  name: {{ include "webapp.fullname" . }}
  labels:
{{ include "webapp.labels" . | indent 4 }}
spec:
  type: {{ .Values.serviceType }}
  ports:
    - port: {{ .Values.servicePort }}`,
			},
		},
		Values: Values{
			"replicaCount": 2,
			"appVersion":   "1.5.0",
			"image":        "myregistry/webapp:1.5.0",
			"serviceType":  "ClusterIP",
			"servicePort":  8080,
		},
	}

	engine := &Engine{}
	result, err := engine.Render(chart, Values{})
	if err != nil {
		fmt.Printf("  렌더링 에러: %v\n", err)
		return
	}

	for name, content := range result {
		fmt.Printf("  --- %s ---\n%s\n\n", name, content)
	}
}

func demoSubchartScoping() {
	fmt.Println("--- 4. 서브차트 값 스코핑 ---")
	fmt.Println("  (서브차트는 자신의 이름에 해당하는 Values 하위 섹션만 받는다)")
	fmt.Println()

	// 메인 차트
	mainChart := &Chart{
		Name: "myapp",
		Templates: []*File{
			{
				Name: "templates/deployment.yaml",
				Data: `# Main chart
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Values.name }}
spec:
  replicas: {{ .Values.replicas }}`,
			},
		},
		Values: Values{
			"name":     "myapp",
			"replicas": 3,
			"redis": map[string]any{
				"architecture": "replication",
				"replicas":     2,
			},
		},
		Dependencies: []*Chart{
			{
				Name: "redis",
				Templates: []*File{
					{
						Name: "templates/statefulset.yaml",
						Data: `# Redis subchart (scoped values)
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: redis
spec:
  replicas: {{ .Values.replicas }}
  # architecture: {{ .Values.architecture }}`,
					},
				},
				Values: Values{
					"architecture": "standalone",
					"replicas":     1,
				},
			},
		},
	}

	engine := &Engine{}
	result, err := engine.Render(mainChart, Values{})
	if err != nil {
		fmt.Printf("  렌더링 에러: %v\n", err)
		return
	}

	for name, content := range result {
		fmt.Printf("  --- %s ---\n%s\n\n", name, content)
	}

	fmt.Println("  주목: redis 서브차트의 replicas=2, architecture=replication")
	fmt.Println("  (부모 Values의 redis.* 섹션이 서브차트 기본값을 오버라이드)")
	fmt.Println()
}

func demoRequired() {
	fmt.Println("--- 5. required 함수와 에러 처리 ---")

	chart := &Chart{
		Name: "strict-app",
		Templates: []*File{
			{
				Name: "templates/secret.yaml",
				Data: `apiVersion: v1
kind: Secret
metadata:
  name: db-credentials
data:
  password: {{ required "DB 비밀번호가 필요합니다 (.Values.dbPassword)" .Values.dbPassword }}`,
			},
		},
		Values: Values{
			"dbPassword": nil,
		},
	}

	engine := &Engine{}
	_, err := engine.Render(chart, Values{})
	if err != nil {
		fmt.Printf("  [예상된 에러] %v\n", err)
	}

	// 값을 제공하면 성공
	chart.Values["dbPassword"] = "s3cret!"
	result, err := engine.Render(chart, Values{})
	if err != nil {
		fmt.Printf("  렌더링 에러: %v\n", err)
		return
	}

	for name, content := range result {
		fmt.Printf("  --- %s (값 제공 시) ---\n%s\n", name, content)
	}

	fmt.Println()
	fmt.Println("=== 템플릿 엔진 PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. Go text/template 기반: 빈 부모 → 모든 템플릿 Parse → ExecuteTemplate")
	fmt.Println("  2. 커스텀 함수 맵: toYaml, toJson, upper, lower, default, quote, indent 등")
	fmt.Println("  3. include 함수: 이름 있는 템플릿을 참조 (재귀 방지 카운터 포함)")
	fmt.Println("  4. 파셜 템플릿: _helpers.tpl 등 _로 시작하면 직접 렌더링 제외")
	fmt.Println("  5. 값 스코핑: 서브차트는 자신의 이름 아래 Values만 받음")
	fmt.Println("  6. required: 필수값 누락 시 명확한 에러 메시지")
}
