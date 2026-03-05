package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// Helm Dry Run PoC
// =============================================================================
//
// 참조: pkg/action/action.go (DryRunStrategy), pkg/action/install.go
//
// Helm의 Dry Run은 세 가지 전략을 제공한다:
//   1. DryRunNone — 실제 설치 (기본)
//   2. DryRunClient — 클라이언트 측 dry-run (렌더링만, 서버 미접촉)
//   3. DryRunServer — 서버 측 dry-run (API 서버에 dry-run 요청)
//
// 이 PoC는 각 전략의 동작 차이를 시뮬레이션한다.
// =============================================================================

// --- DryRunStrategy ---
// Helm 소스: pkg/action/action.go의 DryRunStrategy 타입
type DryRunStrategy string

const (
	// DryRunNone: 실제로 모든 변경을 수행
	DryRunNone DryRunStrategy = "none"

	// DryRunClient: 클라이언트 측 dry-run — 서버에 요청하지 않음
	// helm install --dry-run / helm template
	DryRunClient DryRunStrategy = "client"

	// DryRunServer: 서버 측 dry-run — API 서버에 dry-run 파라미터와 함께 요청
	// helm install --dry-run=server
	DryRunServer DryRunStrategy = "server"
)

func (d DryRunStrategy) String() string { return string(d) }

// isDryRun은 DryRunClient 또는 DryRunServer인지 확인한다.
// Helm 소스: pkg/action/install.go에서 isDryRun 함수로 사용
func isDryRun(d DryRunStrategy) bool {
	return d == DryRunClient || d == DryRunServer
}

// interactWithServer는 서버와 통신하는지 확인한다.
// Helm 소스: pkg/action/install.go에서 interactWithServer 함수로 사용
// DryRunNone 또는 DryRunServer일 때 true (실제 서버 접촉)
func interactWithServer(d DryRunStrategy) bool {
	return d == DryRunNone || d == DryRunServer
}

// --- Chart: 차트 정보 ---
type Chart struct {
	Name     string
	Version  string
	Values   map[string]any
	Template string // 템플릿 내용 (시뮬레이션)
}

// --- Release: 릴리스 정보 ---
type Release struct {
	Name      string
	Namespace string
	Chart     *Chart
	Manifest  string
	Info      ReleaseInfo
	Version   int
	Values    map[string]any
}

type ReleaseInfo struct {
	Status      string
	Description string
	FirstDeployed time.Time
	LastDeployed  time.Time
}

// --- KubeClient: Kubernetes API 클라이언트 (시뮬레이션) ---
type KubeClient struct {
	IsReachable bool
	Resources   map[string]string // name → manifest
}

func NewKubeClient(reachable bool) *KubeClient {
	return &KubeClient{
		IsReachable: reachable,
		Resources:   make(map[string]string),
	}
}

func (k *KubeClient) CheckReachability() error {
	if !k.IsReachable {
		return fmt.Errorf("클러스터에 접근할 수 없음")
	}
	return nil
}

func (k *KubeClient) Create(manifest string, dryRun bool) error {
	if dryRun {
		fmt.Println("    [K8s API] dry-run 요청 — 리소스 검증만 수행, 실제 생성 안 함")
		// 서버 측 dry-run: API 서버가 어드미션 컨트롤러, 스키마 검증 등을 실행
		return k.validateManifest(manifest)
	}
	fmt.Println("    [K8s API] 리소스 생성 요청")
	// 실제 리소스 생성
	k.Resources["created-resource"] = manifest
	return nil
}

func (k *KubeClient) validateManifest(manifest string) error {
	// 간단한 검증 시뮬레이션
	if strings.Contains(manifest, "INVALID") {
		return fmt.Errorf("매니페스트 검증 실패: 유효하지 않은 리소스 정의")
	}
	return nil
}

// --- Install: 설치 액션 ---
// Helm 소스: pkg/action/install.go의 Install 구조체
type Install struct {
	DryRunStrategy DryRunStrategy
	ReleaseName    string
	Namespace      string
	DisableHooks   bool
	HideSecret     bool
	kubeClient     *KubeClient
}

// Run은 설치를 실행한다.
// Helm 소스: pkg/action/install.go의 RunWithContext
// DryRun 전략에 따라 다른 동작을 수행한다.
func (i *Install) Run(chart *Chart, vals map[string]any) (*Release, error) {
	fmt.Printf("\n--- Install.Run (strategy=%s) ---\n", i.DryRunStrategy)

	// Step 1: 서버 접근 가능 여부 확인
	// interactWithServer가 true일 때만 (DryRunNone 또는 DryRunServer)
	if interactWithServer(i.DryRunStrategy) {
		fmt.Println("  [1] 클러스터 접근 가능 여부 확인")
		if err := i.kubeClient.CheckReachability(); err != nil {
			return nil, fmt.Errorf("클러스터 접근 불가: %w", err)
		}
		fmt.Println("      → 클러스터 접근 가능")
	} else {
		fmt.Println("  [1] 클라이언트 모드 — 클러스터 접근 확인 생략")
	}

	// Step 2: HideSecret은 DryRun에서만 사용 가능
	if !isDryRun(i.DryRunStrategy) && i.HideSecret {
		return nil, fmt.Errorf("HideSecret은 dry-run 모드에서만 사용 가능")
	}

	// Step 3: 서버와 통신하지 않으면 mock Capabilities 사용
	// Helm 소스: install.go — !interactWithServer일 때 DefaultCapabilities 설정
	if !interactWithServer(i.DryRunStrategy) {
		fmt.Println("  [2] 클라이언트 모드 — 기본 Capabilities 사용")
		fmt.Println("      KubeVersion: v1.28.0")
		fmt.Println("      APIVersions: apps/v1, v1, batch/v1, ...")
		fmt.Println("      → KubeClient를 PrintingKubeClient로 대체")
	} else {
		fmt.Println("  [2] 서버에서 Capabilities 조회")
	}

	// Step 4: 값 병합 및 템플릿 렌더링
	fmt.Println("  [3] 값 병합 및 템플릿 렌더링")
	manifest := renderManifest(chart, vals)
	fmt.Printf("      → 렌더링된 매니페스트 크기: %d bytes\n", len(manifest))

	// Step 5: 릴리스 객체 생성
	rel := &Release{
		Name:      i.ReleaseName,
		Namespace: i.Namespace,
		Chart:     chart,
		Manifest:  manifest,
		Version:   1,
		Values:    vals,
		Info: ReleaseInfo{
			FirstDeployed: time.Now(),
			LastDeployed:  time.Now(),
		},
	}

	// Step 6: 리소스 빌드
	if interactWithServer(i.DryRunStrategy) {
		fmt.Println("  [4] 리소스 빌드 및 유효성 검사 (API 서버)")
	} else {
		fmt.Println("  [4] 리소스 빌드 (로컬 — OpenAPI 검증 건너뜀 가능)")
	}

	// Step 7: 기존 리소스 충돌 검사
	if interactWithServer(i.DryRunStrategy) {
		fmt.Println("  [5] 기존 리소스 충돌 검사")
	} else {
		fmt.Println("  [5] 충돌 검사 건너뜀 (클라이언트 모드)")
	}

	// Step 8: DryRun이면 여기서 종료
	if isDryRun(i.DryRunStrategy) {
		if i.DryRunStrategy == DryRunServer {
			// 서버 측 dry-run: API 서버에 dry-run 요청
			fmt.Println("  [6] 서버 측 dry-run — API 서버에 dry-run 파라미터로 요청")
			if err := i.kubeClient.Create(manifest, true); err != nil {
				rel.Info.Status = "failed"
				rel.Info.Description = err.Error()
				return rel, err
			}
			fmt.Println("      → 서버 검증 통과")
		} else {
			fmt.Println("  [6] 클라이언트 측 dry-run — 서버 요청 없음")
		}

		rel.Info.Status = "pending-install"
		rel.Info.Description = "Dry run complete"
		fmt.Println("  [결과] Dry run 완료 — 실제 리소스 생성 안 함")
		return rel, nil
	}

	// Step 9: 실제 설치 (DryRunNone)
	fmt.Println("  [6] 실제 리소스 생성")
	if err := i.kubeClient.Create(manifest, false); err != nil {
		rel.Info.Status = "failed"
		rel.Info.Description = err.Error()
		return rel, err
	}

	rel.Info.Status = "deployed"
	rel.Info.Description = "Install complete"
	fmt.Println("  [결과] 설치 완료")

	return rel, nil
}

// renderManifest는 차트를 렌더링한다 (시뮬레이션).
func renderManifest(chart *Chart, vals map[string]any) string {
	// 실제 Helm에서는 Go template 엔진으로 렌더링
	manifest := chart.Template

	// 간단한 변수 치환
	for k, v := range vals {
		placeholder := fmt.Sprintf("{{ .Values.%s }}", k)
		manifest = strings.ReplaceAll(manifest, placeholder, fmt.Sprint(v))
	}

	return manifest
}

func prettyJSON(v any) string {
	b, _ := json.MarshalIndent(v, "    ", "  ")
	return string(b)
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              Helm Dry Run PoC                                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("참조: pkg/action/action.go (DryRunStrategy),")
	fmt.Println("      pkg/action/install.go (RunWithContext)")
	fmt.Println()

	// =================================================================
	// 1. DryRunStrategy 설명
	// =================================================================
	fmt.Println("1. DryRunStrategy 종류")
	fmt.Println(strings.Repeat("-", 60))

	strategies := []struct {
		strategy    DryRunStrategy
		isDry       bool
		interacts   bool
		command     string
		desc        string
	}{
		{DryRunNone, false, true, "helm install my-app ./chart", "실제 설치 수행"},
		{DryRunClient, true, false, "helm install --dry-run my-app ./chart", "클라이언트 측 dry-run"},
		{DryRunServer, true, true, "helm install --dry-run=server my-app ./chart", "서버 측 dry-run"},
	}

	fmt.Printf("  %-8s %-8s %-12s %-45s %s\n",
		"전략", "isDryRun", "서버 통신", "명령어", "설명")
	fmt.Println("  " + strings.Repeat("-", 110))
	for _, s := range strategies {
		fmt.Printf("  %-8s %-8v %-12v %-45s %s\n",
			s.strategy, s.isDry, s.interacts, s.command, s.desc)
	}

	// =================================================================
	// 2. 차트 준비
	// =================================================================
	chart := &Chart{
		Name:    "my-app",
		Version: "1.0.0",
		Values: map[string]any{
			"replicaCount": 1,
			"image":        "nginx:latest",
		},
		Template: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: default
spec:
  replicas: {{ .Values.replicaCount }}
  template:
    spec:
      containers:
      - name: my-app
        image: {{ .Values.image }}
---
apiVersion: v1
kind: Service
metadata:
  name: my-app-svc
  namespace: default
spec:
  type: ClusterIP
  ports:
  - port: 80`,
	}

	vals := map[string]any{
		"replicaCount": 3,
		"image":        "myorg/my-app:v2.0",
	}

	kubeClient := NewKubeClient(true)

	// =================================================================
	// 3. DryRunClient 시나리오
	// =================================================================
	fmt.Println("\n2. DryRunClient 시나리오 (helm install --dry-run)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  클러스터에 접근하지 않고 렌더링 결과만 확인한다.")
	fmt.Println("  helm template과 유사하지만, 릴리스 객체도 생성한다.")

	clientInstall := &Install{
		DryRunStrategy: DryRunClient,
		ReleaseName:    "my-app",
		Namespace:      "default",
		kubeClient:     kubeClient,
	}

	rel, err := clientInstall.Run(chart, vals)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("\n  렌더링된 매니페스트:\n")
		for _, line := range strings.Split(rel.Manifest, "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Printf("\n  릴리스 상태: %s\n", rel.Info.Status)
		fmt.Printf("  설명: %s\n", rel.Info.Description)
	}

	// =================================================================
	// 4. DryRunServer 시나리오
	// =================================================================
	fmt.Println("\n3. DryRunServer 시나리오 (helm install --dry-run=server)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  API 서버에 dry-run 파라미터를 전달하여 서버 측 검증을 수행한다.")
	fmt.Println("  어드미션 컨트롤러, 웹훅, 스키마 검증 등이 실행된다.")

	serverInstall := &Install{
		DryRunStrategy: DryRunServer,
		ReleaseName:    "my-app",
		Namespace:      "default",
		kubeClient:     kubeClient,
	}

	rel, err = serverInstall.Run(chart, vals)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("\n  릴리스 상태: %s\n", rel.Info.Status)
		fmt.Printf("  설명: %s\n", rel.Info.Description)
	}

	// =================================================================
	// 5. DryRunNone 시나리오 (실제 설치)
	// =================================================================
	fmt.Println("\n4. DryRunNone 시나리오 (helm install — 실제 설치)")
	fmt.Println(strings.Repeat("-", 60))

	realInstall := &Install{
		DryRunStrategy: DryRunNone,
		ReleaseName:    "my-app",
		Namespace:      "production",
		kubeClient:     kubeClient,
	}

	rel, err = realInstall.Run(chart, vals)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("\n  릴리스 상태: %s\n", rel.Info.Status)
		fmt.Printf("  설명: %s\n", rel.Info.Description)
	}

	// =================================================================
	// 6. 클러스터 접근 불가 시나리오
	// =================================================================
	fmt.Println("\n5. 클러스터 접근 불가 시나리오")
	fmt.Println(strings.Repeat("-", 60))

	offlineKube := NewKubeClient(false)

	fmt.Println("\n  5a. DryRunServer + 클러스터 접근 불가:")
	offlineServerInstall := &Install{
		DryRunStrategy: DryRunServer,
		ReleaseName:    "my-app",
		Namespace:      "default",
		kubeClient:     offlineKube,
	}
	_, err = offlineServerInstall.Run(chart, vals)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	}

	fmt.Println("\n  5b. DryRunClient + 클러스터 접근 불가:")
	offlineClientInstall := &Install{
		DryRunStrategy: DryRunClient,
		ReleaseName:    "my-app",
		Namespace:      "default",
		kubeClient:     offlineKube,
	}
	rel, err = offlineClientInstall.Run(chart, vals)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Printf("  → 클라이언트 모드는 클러스터 없이도 성공: %s\n", rel.Info.Description)
	}

	// =================================================================
	// 7. 서버 측 검증 실패 시나리오
	// =================================================================
	fmt.Println("\n6. 서버 측 검증 실패 시나리오")
	fmt.Println(strings.Repeat("-", 60))

	invalidChart := &Chart{
		Name:    "bad-chart",
		Version: "0.1.0",
		Template: `apiVersion: apps/v1
kind: INVALID_RESOURCE
metadata:
  name: bad-resource`,
	}

	serverValidateInstall := &Install{
		DryRunStrategy: DryRunServer,
		ReleaseName:    "bad-app",
		Namespace:      "default",
		kubeClient:     kubeClient,
	}

	rel, err = serverValidateInstall.Run(invalidChart, nil)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		fmt.Printf("  → 서버 측 dry-run은 유효하지 않은 리소스를 사전에 감지한다.\n")
	}

	// =================================================================
	// 8. 비교 표
	// =================================================================
	fmt.Println("\n7. DryRun 전략 비교")
	fmt.Println(strings.Repeat("-", 60))

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "  %-25s %-12s %-12s %-12s\n", "동작", "None", "Client", "Server")
	fmt.Fprintf(&buf, "  %s\n", strings.Repeat("-", 65))
	comparisons := []struct {
		action string
		none   string
		client string
		server string
	}{
		{"클러스터 접근 확인", "O", "X", "O"},
		{"Capabilities 조회", "서버", "기본값", "서버"},
		{"템플릿 렌더링", "O", "O", "O"},
		{"JSON Schema 검증", "O", "O", "O"},
		{"리소스 빌드/파싱", "O", "O", "O"},
		{"기존 리소스 충돌 검사", "O", "X", "O"},
		{"어드미션 컨트롤러", "O", "X", "O"},
		{"리소스 생성", "O", "X", "X (dry-run)"},
		{"릴리스 저장 (Secret)", "O", "X", "X"},
		{"훅 실행", "O", "X", "X"},
		{"용도", "실제 배포", "템플릿 확인", "서버 검증"},
	}
	for _, c := range comparisons {
		fmt.Fprintf(&buf, "  %-25s %-12s %-12s %-12s\n", c.action, c.none, c.client, c.server)
	}
	fmt.Print(buf.String())

	// =================================================================
	// 9. 아키텍처 다이어그램
	// =================================================================
	fmt.Println("\n8. Dry Run 아키텍처")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  helm install [--dry-run[=client|server]] <name> <chart>
       │
       v
  ┌──────────────────────────────────────────────────┐
  │  DryRunStrategy 결정                              │
  │                                                    │
  │  --dry-run        → DryRunClient (기본)            │
  │  --dry-run=client → DryRunClient                   │
  │  --dry-run=server → DryRunServer                   │
  │  (없음)           → DryRunNone                     │
  └──────────┬─────────────────────────────────────────┘
             │
             v
  ┌──────────────────────────┐
  │ interactWithServer()?     │
  │ None=Y, Client=N, Server=Y│
  └──────┬──────────┬────────┘
         │ Yes      │ No
         v          v
  ┌──────────┐  ┌──────────────────┐
  │ 클러스터   │  │ DefaultCapabilities│
  │ 접근 확인  │  │ FakeKubeClient    │
  │ Capabilities│ │ MemoryStorage     │
  │ 조회       │  └────────┬─────────┘
  └──────┬───┘           │
         │               │
         v               v
  ┌──────────────────────────┐
  │ 템플릿 렌더링              │
  │ Values 병합 + Go template │
  └──────────┬───────────────┘
             │
             v
  ┌──────────────────────────┐
  │ isDryRun()?               │
  │ Client=Y, Server=Y, None=N│
  └──────┬──────────┬────────┘
         │ Yes      │ No
         v          v
  ┌──────────────┐  ┌──────────────┐
  │ DryRunServer?│  │ 실제 리소스    │
  │  → API 서버에 │  │ 생성          │
  │   dry-run 전송│  │ 훅 실행       │
  │  → 검증만    │  │ 릴리스 저장    │
  │              │  │               │
  │ DryRunClient?│  └──────────────┘
  │  → 즉시 반환 │
  │  → 매니페스트│
  │    출력만    │
  └──────────────┘
`)
}
