// poc-09-applicationset/main.go
//
// Argo CD ApplicationSet 컨트롤러 시뮬레이션
//
// 참조 소스:
//   - applicationset/generators/interface.go   : Generator 인터페이스 정의
//   - applicationset/generators/list.go        : ListGenerator
//   - applicationset/generators/cluster.go     : ClusterGenerator
//   - applicationset/generators/git.go         : GitGenerator
//   - applicationset/generators/matrix.go      : MatrixGenerator (Cartesian product)
//   - applicationset/generators/merge.go       : MergeGenerator (merge by key)
//   - applicationset/controllers/applicationset_controller.go : Reconcile 루프
//
// 핵심 개념:
//   1. Generator 인터페이스: GenerateParams() → []map[string]any
//   2. 5가지 Generator: List, Cluster, Git, Matrix, Merge
//   3. Template 렌더링: params를 Application 템플릿에 적용
//   4. Reconcile 루프: generate → render → create/update/delete
//   5. SyncPolicy: create-only, create-update, create-delete, sync

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"bytes"
)

// ============================================================
// 데이터 모델 (applicationset/generators/interface.go 참조)
// ============================================================

// Generator 인터페이스 — 실제 소스:
//
//	type Generator interface {
//	    GenerateParams(appSetGenerator, applicationSetInfo, client) ([]map[string]any, error)
//	    GetRequeueAfter(appSetGenerator) time.Duration
//	    GetTemplate(appSetGenerator) *ApplicationSetTemplate
//	}
type Generator interface {
	Name() string
	GenerateParams() ([]map[string]any, error)
}

// ApplicationTemplate — Argo CD Application 오브젝트 템플릿
type ApplicationTemplate struct {
	NameTemplate      string // e.g. "{{.env}}-app"
	Namespace         string
	Project           string
	RepoURL           string
	TargetRevision    string
	PathTemplate      string // e.g. "apps/{{.env}}"
	DestinationServer string
}

// RenderedApplication — 렌더링된 Application 오브젝트
type RenderedApplication struct {
	Name              string
	Namespace         string
	Project           string
	RepoURL           string
	TargetRevision    string
	Path              string
	DestinationServer string
}

// SyncPolicy — ApplicationSet이 생성된 Application을 어떻게 관리하는지
// 실제 소스: pkg/apis/application/v1alpha1/applicationset_types.go
type SyncPolicy int

const (
	SyncPolicyCreateOnly   SyncPolicy = iota // 생성만, 수정/삭제 안 함
	SyncPolicyCreateUpdate                   // 생성 + 수정, 삭제 안 함
	SyncPolicyCreateDelete                   // 생성 + 삭제, 수정 안 함
	SyncPolicySync                           // 생성 + 수정 + 삭제 (완전 동기화)
)

func (sp SyncPolicy) String() string {
	switch sp {
	case SyncPolicyCreateOnly:
		return "create-only"
	case SyncPolicyCreateUpdate:
		return "create-update"
	case SyncPolicyCreateDelete:
		return "create-delete"
	case SyncPolicySync:
		return "sync"
	}
	return "unknown"
}

// ============================================================
// Generator 구현 1: ListGenerator
// 참조: applicationset/generators/list.go
//
// 핵심 로직:
//   for i, tmpItem := range appSetGenerator.List.Elements {
//       var element map[string]any
//       json.Unmarshal(tmpItem.Raw, &element)
//       res[i] = element
//   }
// ============================================================

type ListGenerator struct {
	Elements []map[string]any
}

func (g *ListGenerator) Name() string { return "ListGenerator" }

func (g *ListGenerator) GenerateParams() ([]map[string]any, error) {
	result := make([]map[string]any, len(g.Elements))
	for i, elem := range g.Elements {
		// 실제 코드는 json.Unmarshal(tmpItem.Raw, &element) 수행
		// 여기서는 이미 파싱된 map을 복사
		params := make(map[string]any)
		for k, v := range elem {
			params[k] = v
		}
		result[i] = params
	}
	return result, nil
}

// ============================================================
// Generator 구현 2: ClusterGenerator
// 참조: applicationset/generators/cluster.go
//
// 핵심 로직:
//   params["name"]           = string(cluster.Data["name"])
//   params["nameNormalized"] = utils.SanitizeName(name)
//   params["server"]         = string(cluster.Data["server"])
//   params["project"]        = string(cluster.Data["project"])
// ============================================================

type ClusterInfo struct {
	Name    string
	Server  string
	Project string
	Labels  map[string]string
}

type ClusterGenerator struct {
	Clusters []ClusterInfo
}

func (g *ClusterGenerator) Name() string { return "ClusterGenerator" }

func (g *ClusterGenerator) GenerateParams() ([]map[string]any, error) {
	var result []map[string]any
	for _, cluster := range g.Clusters {
		params := map[string]any{
			"name":           cluster.Name,
			"nameNormalized": sanitizeName(cluster.Name),
			"server":         cluster.Server,
			"project":        cluster.Project,
		}
		// metadata.labels 추가 (실제 코드와 동일)
		for k, v := range cluster.Labels {
			params["metadata.labels."+k] = v
		}
		result = append(result, params)
	}
	return result, nil
}

// sanitizeName — utils.SanitizeName() 시뮬레이션
// 실제 소스: applicationset/utils/utils.go
func sanitizeName(name string) string {
	r := strings.NewReplacer(
		"_", "-",
		" ", "-",
		"/", "-",
	)
	return strings.ToLower(r.Replace(name))
}

// ============================================================
// Generator 구현 3: GitGenerator
// 참조: applicationset/generators/git.go
//
// 핵심 로직:
//   - 디렉토리 기반: 저장소의 directories → params
//   - params["path"]           = dirPath
//   - params["path.basename"]  = filepath.Base(dirPath)
//   - params["path.basenameNormalized"] = sanitizeName(basename)
// ============================================================

type GitDirectory struct {
	Path string
}

type GitGenerator struct {
	RepoURL    string
	Revision   string
	Dirs       []GitDirectory // 시뮬레이션용 정적 디렉토리 목록
}

func (g *GitGenerator) Name() string { return "GitGenerator" }

func (g *GitGenerator) GenerateParams() ([]map[string]any, error) {
	var result []map[string]any
	for _, dir := range g.Dirs {
		basename := dir.Path
		if idx := strings.LastIndex(dir.Path, "/"); idx >= 0 {
			basename = dir.Path[idx+1:]
		}
		params := map[string]any{
			"path":                    dir.Path,
			"path.basename":           basename,
			"path.basenameNormalized": sanitizeName(basename),
			"path.filename":           basename,
		}
		result = append(result, params)
	}
	return result, nil
}

// ============================================================
// Generator 구현 4: MatrixGenerator
// 참조: applicationset/generators/matrix.go
//
// 핵심 로직:
//   for _, a := range g0 {
//       for _, b := range g1 {
//           merged := CombineStringMaps(a, b)
//           res = append(res, merged)
//       }
//   }
// (두 Generator의 데카르트 곱)
// ============================================================

type MatrixGenerator struct {
	GeneratorA Generator
	GeneratorB Generator
}

func (g *MatrixGenerator) Name() string { return "MatrixGenerator" }

func (g *MatrixGenerator) GenerateParams() ([]map[string]any, error) {
	// 실제 코드: ErrMoreThanTwoGenerators, ErrLessThanTwoGenerators 검증
	paramsA, err := g.GeneratorA.GenerateParams()
	if err != nil {
		return nil, fmt.Errorf("첫 번째 Generator 오류: %w", err)
	}
	paramsB, err := g.GeneratorB.GenerateParams()
	if err != nil {
		return nil, fmt.Errorf("두 번째 Generator 오류: %w", err)
	}

	var result []map[string]any
	// 데카르트 곱: A의 각 요소 × B의 각 요소
	for _, a := range paramsA {
		for _, b := range paramsB {
			combined := make(map[string]any)
			for k, v := range a {
				combined[k] = v
			}
			for k, v := range b {
				// 실제 코드: utils.CombineStringMaps()는 키 충돌 시 에러 반환
				if _, exists := combined[k]; exists {
					return nil, fmt.Errorf("MatrixGenerator 키 충돌: %q", k)
				}
				combined[k] = v
			}
			result = append(result, combined)
		}
	}
	return result, nil
}

// ============================================================
// Generator 구현 5: MergeGenerator
// 참조: applicationset/generators/merge.go
//
// 핵심 로직:
//   baseParamSetsByMergeKey = getParamSetsByMergeKey(mergeKeys, paramSets[0])
//   for each subsequent generator:
//       for mergeKeyValue, baseParamSet := range baseParamSetsByMergeKey {
//           if overrideParamSet exists → maps.Copy(baseParamSet, overrideParamSet)
//       }
// mergeKey는 JSON으로 직렬화된 복합 키 사용
// ============================================================

type MergeGenerator struct {
	MergeKeys  []string
	Generators []Generator
}

func (g *MergeGenerator) Name() string { return "MergeGenerator" }

func (g *MergeGenerator) GenerateParams() ([]map[string]any, error) {
	if len(g.Generators) < 2 {
		return nil, fmt.Errorf("MergeGenerator는 2개 이상의 Generator가 필요합니다")
	}
	if len(g.MergeKeys) == 0 {
		return nil, fmt.Errorf("MergeGenerator는 최소 1개의 mergeKey가 필요합니다")
	}

	// 1단계: 기본 Generator에서 params 수집
	baseParams, err := g.Generators[0].GenerateParams()
	if err != nil {
		return nil, fmt.Errorf("기본 Generator 오류: %w", err)
	}

	// 2단계: mergeKey로 인덱싱 (실제 코드: getParamSetsByMergeKey)
	baseByKey, err := indexByMergeKey(g.MergeKeys, baseParams)
	if err != nil {
		return nil, err
	}

	// 3단계: 나머지 Generator들로 override
	for i, gen := range g.Generators[1:] {
		overrideParams, err := gen.GenerateParams()
		if err != nil {
			return nil, fmt.Errorf("Generator %d 오류: %w", i+1, err)
		}
		overrideByKey, err := indexByMergeKey(g.MergeKeys, overrideParams)
		if err != nil {
			return nil, err
		}
		// 실제 코드: maps.Copy(baseParamSet, overrideParamSet)
		for key, overrideSet := range overrideByKey {
			if baseSet, exists := baseByKey[key]; exists {
				for k, v := range overrideSet {
					baseSet[k] = v
				}
				baseByKey[key] = baseSet
			}
		}
	}

	// 4단계: 결과 수집
	result := make([]map[string]any, 0, len(baseByKey))
	for _, params := range baseByKey {
		result = append(result, params)
	}
	return result, nil
}

// indexByMergeKey — getParamSetsByMergeKey() 시뮬레이션
// 실제 코드: JSON 직렬화된 키로 맵 구성, 중복 시 ErrNonUniqueParamSets 반환
func indexByMergeKey(mergeKeys []string, paramSets []map[string]any) (map[string]map[string]any, error) {
	result := make(map[string]map[string]any)
	for _, params := range paramSets {
		keyMap := make(map[string]any)
		for _, mk := range mergeKeys {
			keyMap[mk] = params[mk]
		}
		// 실제 코드: json.Marshal(paramSetKey) → 문자열 키
		keyBytes, err := json.Marshal(keyMap)
		if err != nil {
			return nil, fmt.Errorf("mergeKey 직렬화 오류: %w", err)
		}
		keyStr := string(keyBytes)
		if _, exists := result[keyStr]; exists {
			return nil, fmt.Errorf("중복 mergeKey: %s", keyStr)
		}
		// 복사본 저장
		copied := make(map[string]any)
		for k, v := range params {
			copied[k] = v
		}
		result[keyStr] = copied
	}
	return result, nil
}

// ============================================================
// Template 렌더링
// 참조: applicationset/utils/utils.go RenderTemplateParams()
//
// 실제 코드는 text/template 또는 fasttemplate 사용
// params를 Application 템플릿의 각 필드에 적용
// ============================================================

func renderTemplate(tmpl ApplicationTemplate, params map[string]any) (RenderedApplication, error) {
	render := func(s string) (string, error) {
		t, err := template.New("").Delims("{{", "}}").Parse(s)
		if err != nil {
			return "", fmt.Errorf("템플릿 파싱 오류 %q: %w", s, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, params); err != nil {
			return "", fmt.Errorf("템플릿 렌더링 오류 %q: %w", s, err)
		}
		return buf.String(), nil
	}

	name, err := render(tmpl.NameTemplate)
	if err != nil {
		return RenderedApplication{}, err
	}
	path, err := render(tmpl.PathTemplate)
	if err != nil {
		return RenderedApplication{}, err
	}
	destServer, err := render(tmpl.DestinationServer)
	if err != nil {
		return RenderedApplication{}, err
	}

	return RenderedApplication{
		Name:              name,
		Namespace:         tmpl.Namespace,
		Project:           tmpl.Project,
		RepoURL:           tmpl.RepoURL,
		TargetRevision:    tmpl.TargetRevision,
		Path:              path,
		DestinationServer: destServer,
	}, nil
}

// ============================================================
// ApplicationSet Reconcile 루프
// 참조: applicationset/controllers/applicationset_controller.go
//
// Reconcile 흐름:
//   1. ApplicationSet 조회
//   2. Generators 실행 → params 목록
//   3. 각 params로 Template 렌더링 → 희망 Application 목록
//   4. 현재 Application 목록과 비교
//   5. SyncPolicy에 따라 create/update/delete 수행
// ============================================================

type ApplicationSet struct {
	Name       string
	Generator  Generator
	Template   ApplicationTemplate
	SyncPolicy SyncPolicy
}

type ApplicationSetController struct {
	// 현재 클러스터에 있는 Application 상태 (name → app)
	existingApps map[string]RenderedApplication
}

func NewApplicationSetController() *ApplicationSetController {
	return &ApplicationSetController{
		existingApps: make(map[string]RenderedApplication),
	}
}

// Reconcile — ApplicationSet 조정 루프
func (c *ApplicationSetController) Reconcile(appSet ApplicationSet) error {
	fmt.Printf("\n[Reconcile] ApplicationSet=%q Policy=%s\n", appSet.Name, appSet.SyncPolicy)

	// 1단계: Generator 실행
	paramsList, err := appSet.Generator.GenerateParams()
	if err != nil {
		return fmt.Errorf("Generator 실행 오류: %w", err)
	}
	fmt.Printf("  Generator=%s → %d개 param 세트 생성\n", appSet.Generator.Name(), len(paramsList))

	// 2단계: 각 params로 Application 렌더링
	desiredApps := make(map[string]RenderedApplication)
	for i, params := range paramsList {
		app, err := renderTemplate(appSet.Template, params)
		if err != nil {
			return fmt.Errorf("param[%d] 렌더링 오류: %w", i, err)
		}
		desiredApps[app.Name] = app
	}

	// 3단계: SyncPolicy에 따라 create/update/delete 수행
	created, updated, deleted := 0, 0, 0

	// CREATE: 희망 목록에 있지만 현재 없는 것
	for name, app := range desiredApps {
		if _, exists := c.existingApps[name]; !exists {
			c.existingApps[name] = app
			fmt.Printf("  [CREATE] app=%q server=%q path=%q\n",
				app.Name, app.DestinationServer, app.Path)
			created++
		} else if appSet.SyncPolicy == SyncPolicyCreateUpdate || appSet.SyncPolicy == SyncPolicySync {
			// UPDATE
			c.existingApps[name] = app
			fmt.Printf("  [UPDATE] app=%q\n", app.Name)
			updated++
		}
	}

	// DELETE: 현재 있지만 희망 목록에 없는 것
	if appSet.SyncPolicy == SyncPolicyCreateDelete || appSet.SyncPolicy == SyncPolicySync {
		for name := range c.existingApps {
			if _, exists := desiredApps[name]; !exists {
				delete(c.existingApps, name)
				fmt.Printf("  [DELETE] app=%q\n", name)
				deleted++
			}
		}
	}

	fmt.Printf("  결과: created=%d updated=%d deleted=%d total=%d\n",
		created, updated, deleted, len(c.existingApps))
	return nil
}

func (c *ApplicationSetController) ListApps() []RenderedApplication {
	apps := make([]RenderedApplication, 0, len(c.existingApps))
	for _, app := range c.existingApps {
		apps = append(apps, app)
	}
	return apps
}

// ============================================================
// Main: 시나리오 시연
// ============================================================

func main() {
	fmt.Println("=======================================================")
	fmt.Println("Argo CD ApplicationSet 컨트롤러 시뮬레이션")
	fmt.Println("=======================================================")

	ctrl := NewApplicationSetController()

	// ─── 시나리오 1: ListGenerator (3개 환경) ───────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 1: ListGenerator — 3개 환경(dev/staging/prod)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	listAppSet := ApplicationSet{
		Name: "env-appset",
		Generator: &ListGenerator{
			Elements: []map[string]any{
				{"env": "dev",     "namespace": "dev",     "replicas": "1"},
				{"env": "staging", "namespace": "staging", "replicas": "2"},
				{"env": "prod",    "namespace": "prod",    "replicas": "5"},
			},
		},
		Template: ApplicationTemplate{
			NameTemplate:      "{{.env}}-myapp",
			Namespace:         "argocd",
			Project:           "default",
			RepoURL:           "https://github.com/example/myapp",
			TargetRevision:    "HEAD",
			PathTemplate:      "deploy/{{.env}}",
			DestinationServer: "https://kubernetes.default.svc",
		},
		SyncPolicy: SyncPolicyCreateUpdate,
	}

	if err := ctrl.Reconcile(listAppSet); err != nil {
		fmt.Printf("오류: %v\n", err)
	}

	// ─── 시나리오 2: ClusterGenerator (2개 클러스터) ────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 2: ClusterGenerator — 2개 클러스터")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// 실제 코드: argocd.argoproj.io/secret-type=cluster 레이블의 Secret에서 클러스터 목록 조회
	clusterAppSet := ApplicationSet{
		Name: "cluster-appset",
		Generator: &ClusterGenerator{
			Clusters: []ClusterInfo{
				{
					Name:    "us-west-2",
					Server:  "https://k8s-us-west-2.example.com",
					Project: "",
					Labels:  map[string]string{"region": "us-west-2", "env": "prod"},
				},
				{
					Name:    "eu-central-1",
					Server:  "https://k8s-eu-central.example.com",
					Project: "",
					Labels:  map[string]string{"region": "eu-central-1", "env": "prod"},
				},
			},
		},
		Template: ApplicationTemplate{
			NameTemplate:      "{{index . \"name\"}}-guestbook",
			Namespace:         "argocd",
			Project:           "default",
			RepoURL:           "https://github.com/argoproj/argocd-example-apps",
			TargetRevision:    "HEAD",
			PathTemplate:      "guestbook",
			DestinationServer: "{{index . \"server\"}}",
		},
		SyncPolicy: SyncPolicySync,
	}

	if err := ctrl.Reconcile(clusterAppSet); err != nil {
		fmt.Printf("오류: %v\n", err)
	}

	// ─── 시나리오 3: GitGenerator ────────────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 3: GitGenerator — 저장소 디렉토리 기반")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	gitCtrl := NewApplicationSetController()
	gitAppSet := ApplicationSet{
		Name: "git-appset",
		Generator: &GitGenerator{
			RepoURL:  "https://github.com/example/configs",
			Revision: "HEAD",
			Dirs: []GitDirectory{
				{Path: "apps/frontend"},
				{Path: "apps/backend"},
				{Path: "apps/database"},
			},
		},
		Template: ApplicationTemplate{
			NameTemplate:      "{{index . \"path.basename\"}}",
			Namespace:         "argocd",
			Project:           "default",
			RepoURL:           "https://github.com/example/configs",
			TargetRevision:    "HEAD",
			PathTemplate:      "{{index . \"path\"}}",
			DestinationServer: "https://kubernetes.default.svc",
		},
		SyncPolicy: SyncPolicyCreateUpdate,
	}

	if err := gitCtrl.Reconcile(gitAppSet); err != nil {
		fmt.Printf("오류: %v\n", err)
	}

	// ─── 시나리오 4: MatrixGenerator (클러스터 × 환경) ──────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 4: MatrixGenerator — 클러스터 × 환경 데카르트 곱")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	matrixCtrl := NewApplicationSetController()
	matrixAppSet := ApplicationSet{
		Name: "matrix-appset",
		Generator: &MatrixGenerator{
			// GeneratorA: 클러스터 목록
			GeneratorA: &ListGenerator{
				Elements: []map[string]any{
					{"cluster": "us-west-2",   "server": "https://k8s-usw2.example.com"},
					{"cluster": "eu-central-1", "server": "https://k8s-euc1.example.com"},
				},
			},
			// GeneratorB: 환경 목록
			GeneratorB: &ListGenerator{
				Elements: []map[string]any{
					{"env": "staging"},
					{"env": "prod"},
				},
			},
		},
		Template: ApplicationTemplate{
			NameTemplate:      "{{.cluster}}-{{.env}}-app",
			Namespace:         "argocd",
			Project:           "default",
			RepoURL:           "https://github.com/example/app",
			TargetRevision:    "HEAD",
			PathTemplate:      "deploy/{{.env}}",
			DestinationServer: "{{.server}}",
		},
		SyncPolicy: SyncPolicySync,
	}

	if err := matrixCtrl.Reconcile(matrixAppSet); err != nil {
		fmt.Printf("오류: %v\n", err)
	}

	fmt.Println("\n  생성된 Applications:")
	for _, app := range matrixCtrl.ListApps() {
		fmt.Printf("    %-35s → %s\n", app.Name, app.DestinationServer)
	}

	// ─── 시나리오 5: MergeGenerator ─────────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 5: MergeGenerator — 환경별 기본값 + 오버라이드")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	mergeCtrl := NewApplicationSetController()

	// 기본 Generator: 환경별 기본 설정
	baseGen := &ListGenerator{
		Elements: []map[string]any{
			{"env": "dev",  "replicas": "1", "cpu": "100m",  "memory": "128Mi"},
			{"env": "prod", "replicas": "3", "cpu": "500m",  "memory": "512Mi"},
		},
	}
	// 오버라이드 Generator: prod 환경의 일부 값 오버라이드
	overrideGen := &ListGenerator{
		Elements: []map[string]any{
			{"env": "prod", "replicas": "5", "cpu": "1000m"}, // prod만 replicas, cpu 오버라이드
		},
	}

	mergeAppSet := ApplicationSet{
		Name: "merge-appset",
		Generator: &MergeGenerator{
			MergeKeys:  []string{"env"},
			Generators: []Generator{baseGen, overrideGen},
		},
		Template: ApplicationTemplate{
			NameTemplate:      "{{.env}}-app",
			Namespace:         "argocd",
			Project:           "default",
			RepoURL:           "https://github.com/example/app",
			TargetRevision:    "HEAD",
			PathTemplate:      "deploy/{{.env}}",
			DestinationServer: "https://kubernetes.default.svc",
		},
		SyncPolicy: SyncPolicyCreateUpdate,
	}

	if err := mergeCtrl.Reconcile(mergeAppSet); err != nil {
		fmt.Printf("오류: %v\n", err)
	}

	fmt.Println("\n  MergeGenerator 결과 (env 키로 병합):")
	// MergeGenerator 직접 결과 확인
	mg := mergeAppSet.Generator.(*MergeGenerator)
	mergedParams, _ := mg.GenerateParams()
	for _, p := range mergedParams {
		fmt.Printf("    env=%-8s replicas=%-3v cpu=%-6v memory=%v\n",
			p["env"], p["replicas"], p["cpu"], p["memory"])
	}

	// ─── SyncPolicy 변화 시뮬레이션 ─────────────────────────────
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 6: SyncPolicy 변화 — staging 제거 후 sync")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// dev, staging, prod → dev, prod (staging 제거)
	reducedListAppSet := listAppSet
	reducedListAppSet.SyncPolicy = SyncPolicySync
	reducedListAppSet.Generator = &ListGenerator{
		Elements: []map[string]any{
			{"env": "dev",  "namespace": "dev",  "replicas": "1"},
			{"env": "prod", "namespace": "prod", "replicas": "5"},
		},
	}

	if err := ctrl.Reconcile(reducedListAppSet); err != nil {
		fmt.Printf("오류: %v\n", err)
	}

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("\n핵심 개념 요약:")
	fmt.Println("  - Generator.GenerateParams() → []map[string]any")
	fmt.Println("  - MatrixGenerator: 두 Generator의 데카르트 곱 (N×M)")
	fmt.Println("  - MergeGenerator: mergeKey로 params를 병합 (JSON 키 인덱싱)")
	fmt.Println("  - SyncPolicy: create-only < create-update < create-delete < sync")
	fmt.Println("  - Reconcile 루프: generate → render → diff → apply")
}
