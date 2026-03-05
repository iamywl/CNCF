// Helm v4 데이터 모델 PoC: Chart, Release, Values, Hook 구조체
//
// 이 PoC는 Helm v4의 핵심 데이터 모델을 시뮬레이션합니다:
//   1. Chart 구조체 (pkg/chart/v2/chart.go) - 차트 메타데이터, 템플릿, 의존성
//   2. Metadata 구조체 (pkg/chart/v2/metadata.go) - Chart.yaml 내용
//   3. Release 구조체 (pkg/release/v1/release.go) - 배포된 릴리스 정보
//   4. Values (pkg/chart/common/values.go) - 중첩 map, 점 표기법 경로 탐색
//   5. Hook 구조체 (pkg/release/v1/hook.go) - 라이프사이클 훅
//   6. Info/Status (pkg/release/v1/info.go, common/status.go)
//
// 실행: go run main.go

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// Values: Helm의 pkg/chart/common/values.go
// chart의 기본값 + 사용자 오버라이드 값을 표현하는 핵심 타입.
// map[string]any 기반이며, 점(.) 표기법으로 중첩 값에 접근 가능.
// =============================================================================

// Values는 차트 설정 값들의 컬렉션이다.
// 실제 Helm: type Values map[string]any
type Values map[string]any

// Table은 점 표기법으로 중첩된 하위 테이블을 반환한다.
// 예: v.Table("global.image") → global 안의 image 테이블
// 실제 Helm: (v Values) Table(name string) (Values, error)
func (v Values) Table(name string) (Values, error) {
	table := v
	var err error
	for _, n := range strings.Split(name, ".") {
		table, err = tableLookup(table, n)
		if err != nil {
			return nil, err
		}
	}
	return table, nil
}

// PathValue는 점 표기법 경로를 따라 최종 값을 반환한다.
// 예: v.PathValue("image.tag") → "1.21"
// 실제 Helm: (v Values) PathValue(path string) (any, error)
func (v Values) PathValue(path string) (any, error) {
	parts := strings.Split(path, ".")
	if len(parts) == 1 {
		val, ok := v[parts[0]]
		if !ok {
			return nil, fmt.Errorf("키 %q 를 찾을 수 없습니다", parts[0])
		}
		return val, nil
	}

	// 마지막 키를 제외한 경로로 테이블 탐색
	tablePath := strings.Join(parts[:len(parts)-1], ".")
	table, err := v.Table(tablePath)
	if err != nil {
		return nil, err
	}

	key := parts[len(parts)-1]
	val, ok := table[key]
	if !ok {
		return nil, fmt.Errorf("키 %q 를 찾을 수 없습니다", key)
	}
	return val, nil
}

// AsMap은 Values를 map[string]any로 반환한다.
// nil 방지용 유틸리티.
func (v Values) AsMap() map[string]any {
	if len(v) == 0 {
		return map[string]any{}
	}
	return v
}

func tableLookup(v Values, key string) (Values, error) {
	val, ok := v[key]
	if !ok {
		return nil, fmt.Errorf("테이블 %q 를 찾을 수 없습니다", key)
	}
	if m, ok := val.(map[string]any); ok {
		return Values(m), nil
	}
	if m, ok := val.(Values); ok {
		return m, nil
	}
	return nil, fmt.Errorf("%q 는 테이블이 아닙니다", key)
}

// =============================================================================
// File: Helm의 pkg/chart/common/file.go
// 차트 내의 개별 파일(템플릿, README 등)을 표현
// =============================================================================

// File은 차트 아카이브 내의 파일을 표현한다.
// 실제 Helm: common.File{Name, Data}
type File struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// =============================================================================
// Metadata: Helm의 pkg/chart/v2/metadata.go
// Chart.yaml 파일의 내용을 구조화. 이름, 버전, 의존성, 유효성 검사 포함.
// =============================================================================

// Dependency는 차트의 의존성을 표현한다.
// 실제 Helm: v2.Dependency
type Dependency struct {
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	Repository string `json:"repository,omitempty"`
	Condition  string `json:"condition,omitempty"`
	Alias      string `json:"alias,omitempty"`
}

// Maintainer는 차트 관리자를 표현한다.
type Maintainer struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Metadata는 Chart.yaml의 내용을 모델링한다.
// 실제 Helm: v2.Metadata
type Metadata struct {
	APIVersion   string        `json:"apiVersion"`
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	Description  string        `json:"description,omitempty"`
	Type         string        `json:"type,omitempty"` // application 또는 library
	AppVersion   string        `json:"appVersion,omitempty"`
	Home         string        `json:"home,omitempty"`
	Sources      []string      `json:"sources,omitempty"`
	Keywords     []string      `json:"keywords,omitempty"`
	KubeVersion  string        `json:"kubeVersion,omitempty"`
	Dependencies []*Dependency `json:"dependencies,omitempty"`
	Maintainers  []*Maintainer `json:"maintainers,omitempty"`
	Deprecated   bool          `json:"deprecated,omitempty"`
}

// Validate는 메타데이터의 유효성을 검사한다.
// 실제 Helm: (md *Metadata) Validate() error
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
	if md.Type != "" && md.Type != "application" && md.Type != "library" {
		return fmt.Errorf("chart.metadata.type은 application 또는 library여야 합니다")
	}
	return nil
}

// =============================================================================
// Chart: Helm의 pkg/chart/v2/chart.go
// Helm 패키지의 핵심 구조체. 메타데이터, 템플릿, 기본값, 의존성 트리 포함.
// =============================================================================

// Chart는 Helm 패키지를 표현한다.
// 실제 Helm: v2.Chart{Raw, Metadata, Lock, Templates, Values, Schema, Files, ...}
type Chart struct {
	Raw       []*File        `json:"-"`
	Metadata  *Metadata      `json:"metadata"`
	Templates []*File        `json:"templates"`
	Values    map[string]any `json:"values"`
	Schema    []byte         `json:"schema,omitempty"`
	Files     []*File        `json:"files"`

	parent       *Chart
	dependencies []*Chart
}

// Name은 차트 이름을 반환한다.
func (ch *Chart) Name() string {
	if ch.Metadata == nil {
		return ""
	}
	return ch.Metadata.Name
}

// IsRoot는 루트 차트인지 확인한다.
func (ch *Chart) IsRoot() bool { return ch.parent == nil }

// Parent는 부모 차트를 반환한다.
func (ch *Chart) Parent() *Chart { return ch.parent }

// Dependencies는 의존성 차트 목록을 반환한다.
func (ch *Chart) Dependencies() []*Chart { return ch.dependencies }

// AddDependency는 의존성 차트를 추가하며, parent를 설정한다.
// 실제 Helm: (ch *Chart) AddDependency(charts ...*Chart)
func (ch *Chart) AddDependency(charts ...*Chart) {
	for _, c := range charts {
		c.parent = ch
		ch.dependencies = append(ch.dependencies, c)
	}
}

// ChartPath는 점(.) 표기법으로 차트 경로를 반환한다.
// 예: "myapp.redis"
func (ch *Chart) ChartPath() string {
	if ch.IsRoot() {
		return ch.Name()
	}
	return ch.Parent().ChartPath() + "." + ch.Name()
}

// ChartFullPath는 파일시스템 스타일 경로를 반환한다.
// 예: "myapp/charts/redis"
func (ch *Chart) ChartFullPath() string {
	if ch.IsRoot() {
		return ch.Name()
	}
	return ch.Parent().ChartFullPath() + "/charts/" + ch.Name()
}

// Validate는 차트의 유효성을 검사한다.
func (ch *Chart) Validate() error {
	return ch.Metadata.Validate()
}

// =============================================================================
// Status: Helm의 pkg/release/common/status.go
// 릴리스의 현재 상태를 표현. 배포/삭제/실패 등 9개 상태.
// =============================================================================

// Status는 릴리스의 현재 상태이다.
type Status string

const (
	StatusUnknown        Status = "unknown"
	StatusDeployed       Status = "deployed"
	StatusUninstalled    Status = "uninstalled"
	StatusSuperseded     Status = "superseded"
	StatusFailed         Status = "failed"
	StatusUninstalling   Status = "uninstalling"
	StatusPendingInstall Status = "pending-install"
	StatusPendingUpgrade Status = "pending-upgrade"
	StatusPendingRollback Status = "pending-rollback"
)

// IsPending는 전이 상태인지 확인한다.
func (s Status) IsPending() bool {
	return s == StatusPendingInstall || s == StatusPendingUpgrade || s == StatusPendingRollback
}

// =============================================================================
// Hook: Helm의 pkg/release/v1/hook.go
// 릴리스 라이프사이클의 특정 시점에 실행되는 훅.
// =============================================================================

type HookEvent string

const (
	HookPreInstall   HookEvent = "pre-install"
	HookPostInstall  HookEvent = "post-install"
	HookPreDelete    HookEvent = "pre-delete"
	HookPostDelete   HookEvent = "post-delete"
	HookPreUpgrade   HookEvent = "pre-upgrade"
	HookPostUpgrade  HookEvent = "post-upgrade"
	HookPreRollback  HookEvent = "pre-rollback"
	HookPostRollback HookEvent = "post-rollback"
	HookTest         HookEvent = "test"
)

type HookDeletePolicy string

const (
	HookSucceeded          HookDeletePolicy = "hook-succeeded"
	HookFailed             HookDeletePolicy = "hook-failed"
	HookBeforeHookCreation HookDeletePolicy = "before-hook-creation"
)

type HookPhase string

const (
	HookPhaseUnknown   HookPhase = "Unknown"
	HookPhaseRunning   HookPhase = "Running"
	HookPhaseSucceeded HookPhase = "Succeeded"
	HookPhaseFailed    HookPhase = "Failed"
)

// Hook은 릴리스 훅을 표현한다.
type Hook struct {
	Name           string             `json:"name"`
	Kind           string             `json:"kind"`
	Path           string             `json:"path"`
	Manifest       string             `json:"manifest"`
	Events         []HookEvent        `json:"events"`
	Weight         int                `json:"weight"`
	DeletePolicies []HookDeletePolicy `json:"delete_policies"`
	LastRun        HookExecution      `json:"last_run"`
}

type HookExecution struct {
	StartedAt   time.Time `json:"started_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	Phase       HookPhase `json:"phase"`
}

// =============================================================================
// Info: Helm의 pkg/release/v1/info.go
// 릴리스의 배포 시간, 상태, 노트 등 메타정보.
// =============================================================================

// Info는 릴리스 정보를 담는다.
type Info struct {
	FirstDeployed time.Time `json:"first_deployed,omitempty"`
	LastDeployed  time.Time `json:"last_deployed,omitempty"`
	Deleted       time.Time `json:"deleted,omitempty"`
	Description   string    `json:"description,omitempty"`
	Status        Status    `json:"status"`
	Notes         string    `json:"notes,omitempty"`
}

// =============================================================================
// Release: Helm의 pkg/release/v1/release.go
// 차트의 배포 인스턴스. 이름, 버전(리비전), 상태, 매니페스트 등 포함.
// =============================================================================

// Release는 차트의 배포를 표현한다.
// 실제 Helm: v1.Release{Name, Info, Chart, Config, Manifest, Hooks, Version, Namespace, Labels}
type Release struct {
	Name      string         `json:"name"`
	Info      *Info          `json:"info"`
	Chart     *Chart         `json:"chart,omitempty"`
	Config    map[string]any `json:"config,omitempty"`
	Manifest  string         `json:"manifest,omitempty"`
	Hooks     []*Hook        `json:"hooks,omitempty"`
	Version   int            `json:"version"`
	Namespace string         `json:"namespace"`
	Labels    map[string]string `json:"-"`
}

// SetStatus는 릴리스의 상태를 설정하는 헬퍼이다.
// 실제 Helm: (r *Release) SetStatus(status, msg)
func (r *Release) SetStatus(status Status, msg string) {
	r.Info.Status = status
	r.Info.Description = msg
}

// =============================================================================
// main: 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== Helm v4 데이터 모델 PoC ===")
	fmt.Println()

	// 1) Values 사용법 시연
	demoValues()

	// 2) Chart 구조체 + 의존성 트리
	chart := demoChart()

	// 3) Release 구조체
	demoRelease(chart)

	// 4) Hook 시스템
	demoHooks()

	// 5) JSON 직렬화/역직렬화
	demoSerialization()
}

func demoValues() {
	fmt.Println("--- 1. Values (중첩 맵 + 점 표기법 경로 탐색) ---")

	vals := Values{
		"replicaCount": 3,
		"image": map[string]any{
			"repository": "nginx",
			"tag":        "1.21",
			"pullPolicy": "IfNotPresent",
		},
		"service": map[string]any{
			"type": "ClusterIP",
			"port": 80,
		},
		"global": map[string]any{
			"storageClass": "gp2",
			"image": map[string]any{
				"registry": "docker.io",
			},
		},
	}

	// PathValue로 중첩 값 접근
	tag, _ := vals.PathValue("image.tag")
	fmt.Printf("  image.tag = %v\n", tag)

	registry, _ := vals.PathValue("global.image.registry")
	fmt.Printf("  global.image.registry = %v\n", registry)

	replicas, _ := vals.PathValue("replicaCount")
	fmt.Printf("  replicaCount = %v\n", replicas)

	// Table로 하위 섹션 가져오기
	imageTable, _ := vals.Table("image")
	fmt.Printf("  image 테이블: %v\n", imageTable)

	// 존재하지 않는 키
	_, err := vals.PathValue("nonexistent.key")
	fmt.Printf("  nonexistent.key → error: %v\n", err)

	fmt.Println()
}

func demoChart() *Chart {
	fmt.Println("--- 2. Chart 구조체 + 의존성 트리 ---")

	// 루트 차트 생성
	rootChart := &Chart{
		Metadata: &Metadata{
			APIVersion:  "v2",
			Name:        "myapp",
			Version:     "1.0.0",
			Description: "My Application Chart",
			Type:        "application",
			AppVersion:  "2.0.0",
			KubeVersion: ">=1.20.0",
			Keywords:    []string{"web", "nginx"},
			Maintainers: []*Maintainer{
				{Name: "Developer", Email: "dev@example.com"},
			},
			Dependencies: []*Dependency{
				{Name: "redis", Version: "17.x", Repository: "https://charts.bitnami.com"},
				{Name: "postgresql", Version: "12.x", Repository: "oci://registry.example.com"},
			},
		},
		Values: map[string]any{
			"replicaCount": 1,
			"image":        map[string]any{"repository": "myapp", "tag": "latest"},
		},
		Templates: []*File{
			{Name: "templates/deployment.yaml", Data: []byte("apiVersion: apps/v1\nkind: Deployment...")},
			{Name: "templates/service.yaml", Data: []byte("apiVersion: v1\nkind: Service...")},
			{Name: "templates/_helpers.tpl", Data: []byte("{{- define \"myapp.name\" -}}...")},
		},
		Files: []*File{
			{Name: "README.md", Data: []byte("# MyApp Chart")},
		},
	}

	// 서브차트 (의존성)
	redisChart := &Chart{
		Metadata: &Metadata{
			APIVersion: "v2", Name: "redis", Version: "17.3.14",
			Type: "application",
		},
		Values: map[string]any{"architecture": "standalone"},
		Templates: []*File{
			{Name: "templates/statefulset.yaml", Data: []byte("kind: StatefulSet...")},
		},
	}

	postgresChart := &Chart{
		Metadata: &Metadata{
			APIVersion: "v2", Name: "postgresql", Version: "12.1.9",
			Type: "application",
		},
		Values: map[string]any{"auth": map[string]any{"postgresPassword": ""}},
	}

	// 의존성 트리 구성
	rootChart.AddDependency(redisChart, postgresChart)

	// 유효성 검사
	if err := rootChart.Validate(); err != nil {
		fmt.Printf("  유효성 검사 실패: %v\n", err)
	} else {
		fmt.Printf("  유효성 검사 통과: %s v%s\n", rootChart.Name(), rootChart.Metadata.Version)
	}

	// 트리 구조 출력
	fmt.Printf("  차트 트리:\n")
	printChartTree(rootChart, "    ")

	// ChartPath / ChartFullPath
	fmt.Printf("\n  경로 표기:\n")
	fmt.Printf("    rootChart.ChartPath()     = %s\n", rootChart.ChartPath())
	fmt.Printf("    redis.ChartPath()         = %s\n", redisChart.ChartPath())
	fmt.Printf("    redis.ChartFullPath()     = %s\n", redisChart.ChartFullPath())
	fmt.Printf("    postgresql.ChartFullPath() = %s\n", postgresChart.ChartFullPath())
	fmt.Printf("    redis.IsRoot()            = %v\n", redisChart.IsRoot())
	fmt.Printf("    rootChart.IsRoot()        = %v\n", rootChart.IsRoot())

	fmt.Println()
	return rootChart
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

func demoRelease(ch *Chart) {
	fmt.Println("--- 3. Release 구조체 ---")

	now := time.Now()
	rel := &Release{
		Name: "myapp-prod",
		Info: &Info{
			FirstDeployed: now.Add(-24 * time.Hour),
			LastDeployed:  now,
			Status:        StatusDeployed,
			Description:   "Upgrade complete",
			Notes:         "Application is running at http://myapp.example.com",
		},
		Chart:     ch,
		Config:    map[string]any{"replicaCount": 3, "image": map[string]any{"tag": "v2.1.0"}},
		Version:   3,
		Namespace: "production",
		Manifest:  "---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: myapp-prod\n...",
		Labels:    map[string]string{"owner": "helm", "name": "myapp-prod"},
	}

	fmt.Printf("  릴리스: %s (v%d)\n", rel.Name, rel.Version)
	fmt.Printf("  네임스페이스: %s\n", rel.Namespace)
	fmt.Printf("  상태: %s\n", rel.Info.Status)
	fmt.Printf("  설명: %s\n", rel.Info.Description)
	fmt.Printf("  차트: %s v%s\n", rel.Chart.Name(), rel.Chart.Metadata.Version)
	fmt.Printf("  Config: %v\n", rel.Config)
	fmt.Printf("  노트: %s\n", rel.Info.Notes)
	fmt.Printf("  최초 배포: %s\n", rel.Info.FirstDeployed.Format(time.RFC3339))
	fmt.Printf("  최종 배포: %s\n", rel.Info.LastDeployed.Format(time.RFC3339))

	// 상태 전이 시연
	fmt.Printf("\n  상태 전이:\n")
	for _, s := range []Status{StatusPendingInstall, StatusDeployed, StatusPendingUpgrade, StatusSuperseded, StatusFailed} {
		fmt.Printf("    %s → IsPending: %v\n", s, s.IsPending())
	}

	fmt.Println()
}

func demoHooks() {
	fmt.Println("--- 4. Hook 시스템 ---")

	hooks := []*Hook{
		{
			Name:     "myapp-test",
			Kind:     "Pod",
			Path:     "templates/tests/test-connection.yaml",
			Events:   []HookEvent{HookTest},
			Weight:   0,
			Manifest: "apiVersion: v1\nkind: Pod\nmetadata:\n  name: myapp-test...",
		},
		{
			Name:           "myapp-pre-install",
			Kind:           "Job",
			Path:           "templates/pre-install-job.yaml",
			Events:         []HookEvent{HookPreInstall},
			Weight:         -5,
			DeletePolicies: []HookDeletePolicy{HookBeforeHookCreation},
			Manifest:       "apiVersion: batch/v1\nkind: Job...",
		},
		{
			Name:     "myapp-post-install",
			Kind:     "Job",
			Path:     "templates/post-install-job.yaml",
			Events:   []HookEvent{HookPostInstall, HookPostUpgrade},
			Weight:   10,
			Manifest: "apiVersion: batch/v1\nkind: Job...",
			LastRun: HookExecution{
				StartedAt:   time.Now().Add(-5 * time.Minute),
				CompletedAt: time.Now().Add(-4 * time.Minute),
				Phase:       HookPhaseSucceeded,
			},
		},
	}

	for _, h := range hooks {
		fmt.Printf("  Hook: %s (Kind: %s, Weight: %d)\n", h.Name, h.Kind, h.Weight)
		fmt.Printf("    이벤트: %v\n", h.Events)
		if len(h.DeletePolicies) > 0 {
			fmt.Printf("    삭제 정책: %v\n", h.DeletePolicies)
		}
		if h.LastRun.Phase != "" {
			fmt.Printf("    최종 실행: %s (%s)\n", h.LastRun.Phase, h.LastRun.CompletedAt.Format(time.RFC3339))
		}
	}

	fmt.Println()
}

func demoSerialization() {
	fmt.Println("--- 5. JSON 직렬화/역직렬화 ---")

	// Release를 JSON으로 직렬화
	rel := &Release{
		Name: "test-release",
		Info: &Info{
			FirstDeployed: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			LastDeployed:  time.Date(2024, 6, 20, 14, 0, 0, 0, time.UTC),
			Status:        StatusDeployed,
			Description:   "Install complete",
		},
		Config:    map[string]any{"replicaCount": 2},
		Version:   1,
		Namespace: "default",
		Hooks: []*Hook{
			{
				Name:   "pre-install-hook",
				Kind:   "Job",
				Events: []HookEvent{HookPreInstall},
				Weight: -1,
			},
		},
	}

	data, err := json.MarshalIndent(rel, "  ", "  ")
	if err != nil {
		fmt.Printf("  직렬화 실패: %v\n", err)
		return
	}
	fmt.Printf("  JSON 출력:\n  %s\n\n", string(data))

	// JSON → Release 역직렬화
	var restored Release
	if err := json.Unmarshal(data, &restored); err != nil {
		fmt.Printf("  역직렬화 실패: %v\n", err)
		return
	}
	fmt.Printf("  복원된 릴리스: %s v%d, 상태=%s\n", restored.Name, restored.Version, restored.Info.Status)
	fmt.Printf("  Config: %v\n", restored.Config)
	fmt.Printf("  Hooks: %d개\n", len(restored.Hooks))

	fmt.Println()
	fmt.Println("=== 데이터 모델 PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. Values: map[string]any 기반, 점 표기법으로 중첩 접근 (Table, PathValue)")
	fmt.Println("  2. Chart: 메타데이터 + 템플릿 + 기본값 + 의존성 트리 (parent/child)")
	fmt.Println("  3. Release: 차트 배포 인스턴스 (이름 + 버전(리비전) + 상태 + 매니페스트)")
	fmt.Println("  4. Hook: 라이프사이클 이벤트(pre/post install/upgrade) + 가중치 + 삭제 정책")
	fmt.Println("  5. Status: 9개 상태 (deployed/superseded/failed/pending-*/uninstalled/...)")
}
