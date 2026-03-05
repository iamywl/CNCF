// poc-02-data-model/main.go
//
// Argo CD 핵심 데이터 모델 시뮬레이션
//
// 핵심 개념:
//   - Application CRD: Spec(Source, Destination, SyncPolicy), Status(Sync, Health, Resources), Operation
//   - AppProject CRD: SourceRepos, Destinations, Roles, SyncWindows
//   - ApplicationSet: Generators(List, Cluster), Template
//   - Cluster 타입 (K8s Secret with label)
//   - Repository 타입 (K8s Secret)
//   - Health 모델: HealthStatusCode 순서 및 IsWorse() 함수
//   - 데이터 생명주기: App 생성 → OutOfSync → Sync → Healthy
//
// 실행: go run main.go

package main

import (
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// 1. Health 모델
//    소스: pkg/apis/application/v1alpha1/types.go — HealthStatusCode
// =============================================================================

// HealthStatusCode는 리소스 또는 애플리케이션의 헬스 상태를 나타낸다.
// 소스: pkg/apis/application/v1alpha1/types.go
//
//	const (
//	    HealthStatusUnknown     HealthStatusCode = "Unknown"
//	    HealthStatusProgressing HealthStatusCode = "Progressing"
//	    HealthStatusHealthy     HealthStatusCode = "Healthy"
//	    HealthStatusSuspended   HealthStatusCode = "Suspended"
//	    HealthStatusDegraded    HealthStatusCode = "Degraded"
//	    HealthStatusMissing     HealthStatusCode = "Missing"
//	)
type HealthStatusCode string

const (
	HealthStatusUnknown     HealthStatusCode = "Unknown"
	HealthStatusProgressing HealthStatusCode = "Progressing"
	HealthStatusHealthy     HealthStatusCode = "Healthy"
	HealthStatusSuspended   HealthStatusCode = "Suspended"
	HealthStatusDegraded    HealthStatusCode = "Degraded"
	HealthStatusMissing     HealthStatusCode = "Missing"
)

// healthOrder는 IsWorse() 비교를 위한 상태 심각도 순서다.
// 숫자가 클수록 더 나쁜 상태 (Healthy=0이 가장 좋음)
// 소스: pkg/apis/application/v1alpha1/types.go — IsWorse()
var healthOrder = map[HealthStatusCode]int{
	HealthStatusHealthy:     0,
	HealthStatusSuspended:   1,
	HealthStatusProgressing: 2,
	HealthStatusMissing:     3,
	HealthStatusDegraded:    4,
	HealthStatusUnknown:     5,
}

// HealthStatus는 헬스 상태와 메시지를 담는 구조체다.
// 소스: pkg/apis/application/v1alpha1/types.go
type HealthStatus struct {
	Status  HealthStatusCode `json:"status,omitempty"`
	Message string           `json:"message,omitempty"`
}

// IsWorse는 현재 상태가 other보다 나쁜지 판단한다.
// 소스: pkg/apis/application/v1alpha1/types.go — IsWorse()
//
//	func IsWorse(current, other HealthStatusCode) bool {
//	    ...
//	    return healthOrder[current] > healthOrder[other]
//	}
//
// 이 함수는 여러 리소스의 헬스를 집계할 때 사용된다.
// App의 전체 헬스 = 소속 리소스 중 가장 나쁜 상태
func IsWorse(current, other HealthStatusCode) bool {
	currentOrder, ok1 := healthOrder[current]
	otherOrder, ok2 := healthOrder[other]
	if !ok1 {
		currentOrder = healthOrder[HealthStatusUnknown]
	}
	if !ok2 {
		otherOrder = healthOrder[HealthStatusUnknown]
	}
	return currentOrder > otherOrder
}

// =============================================================================
// 2. SyncStatus 모델
//    소스: pkg/apis/application/v1alpha1/types.go — SyncStatusCode
// =============================================================================

// SyncStatusCode는 App과 Git 상태 일치 여부를 나타낸다.
type SyncStatusCode string

const (
	SyncStatusCodeUnknown   SyncStatusCode = "Unknown"
	SyncStatusCodeSynced    SyncStatusCode = "Synced"
	SyncStatusCodeOutOfSync SyncStatusCode = "OutOfSync"
)

// SyncStatus는 동기화 상태 정보다.
// 소스: pkg/apis/application/v1alpha1/types.go
type SyncStatus struct {
	Status    SyncStatusCode `json:"status"`
	Revision  string         `json:"revision,omitempty"`
	Revisions []string       `json:"revisions,omitempty"` // 멀티소스 앱
}

// =============================================================================
// 3. Application CRD
//    소스: pkg/apis/application/v1alpha1/types.go
// =============================================================================

// ApplicationSource는 Git 레포지토리와 배포 도구 설정이다.
// 소스: pkg/apis/application/v1alpha1/types.go — ApplicationSource
type ApplicationSource struct {
	// Git 레포지토리 URL
	RepoURL string `json:"repoURL"`
	// 레포지토리 내 앱 경로
	Path string `json:"path,omitempty"`
	// Git 브랜치, 태그, 커밋 SHA
	TargetRevision string `json:"targetRevision,omitempty"`
	// Helm 설정 (Source와 상호 배타적이 아님 — 경로가 Helm chart인 경우 함께 사용)
	Helm *ApplicationSourceHelm `json:"helm,omitempty"`
	// Kustomize 설정
	Kustomize *ApplicationSourceKustomize `json:"kustomize,omitempty"`
	// 소스 타입 (빈 문자열이면 자동 감지)
	Chart string `json:"chart,omitempty"` // Helm repo의 chart 이름
}

// ApplicationSourceHelm은 Helm 특화 설정이다.
type ApplicationSourceHelm struct {
	ReleaseName string            `json:"releaseName,omitempty"`
	Values      string            `json:"values,omitempty"`
	ValueFiles  []string          `json:"valueFiles,omitempty"`
	Parameters  []HelmParameter   `json:"parameters,omitempty"`
}

// HelmParameter는 Helm --set 파라미터다.
type HelmParameter struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ApplicationSourceKustomize는 Kustomize 특화 설정이다.
type ApplicationSourceKustomize struct {
	NamePrefix  string            `json:"namePrefix,omitempty"`
	NameSuffix  string            `json:"nameSuffix,omitempty"`
	Images      []string          `json:"images,omitempty"` // newTag 형식
	CommonLabels map[string]string `json:"commonLabels,omitempty"`
}

// ApplicationDestination은 배포 대상 클러스터와 네임스페이스다.
// 소스: pkg/apis/application/v1alpha1/types.go — ApplicationDestination
type ApplicationDestination struct {
	// 대상 클러스터 API Server URL (in-cluster이면 "https://kubernetes.default.svc")
	Server    string `json:"server,omitempty"`
	Namespace string `json:"namespace"`
	// 클러스터 이름 (Server 대신 사용 가능)
	Name string `json:"name,omitempty"`
}

// SyncPolicy는 자동 동기화 설정이다.
// 소스: pkg/apis/application/v1alpha1/types.go — SyncPolicy
type SyncPolicy struct {
	// Automated가 nil이 아니면 자동 동기화 활성화
	Automated *SyncPolicyAutomated `json:"automated,omitempty"`
	// Sync 옵션 (--server-side-apply, --replace 등)
	SyncOptions []string `json:"syncOptions,omitempty"`
	// Retry 설정
	Retry *RetryStrategy `json:"retry,omitempty"`
}

// SyncPolicyAutomated는 자동 동기화 세부 설정이다.
type SyncPolicyAutomated struct {
	// Prune: Git에 없는 리소스를 클러스터에서 삭제
	Prune bool `json:"prune,omitempty"`
	// SelfHeal: 클러스터 상태가 Git과 달라지면 자동 복구
	SelfHeal bool `json:"selfHeal,omitempty"`
	// AllowEmpty: 소스가 빈 디렉토리여도 sync 허용
	AllowEmpty bool `json:"allowEmpty,omitempty"`
}

// RetryStrategy는 sync 실패 시 재시도 전략이다.
type RetryStrategy struct {
	Limit   int64    `json:"limit,omitempty"`  // 최대 재시도 횟수 (-1이면 무제한)
	Backoff *Backoff `json:"backoff,omitempty"`
}

// Backoff는 지수 백오프 설정이다.
type Backoff struct {
	Duration    string `json:"duration,omitempty"`    // 기본 대기 시간 (예: "5s")
	Factor      *int64 `json:"factor,omitempty"`      // 배율 (예: 2)
	MaxDuration string `json:"maxDuration,omitempty"` // 최대 대기 시간 (예: "3m")
}

// ApplicationSpec은 App의 원하는 상태를 정의한다.
// 소스: pkg/apis/application/v1alpha1/types.go — ApplicationSpec
type ApplicationSpec struct {
	// 단일 소스 (Source와 Sources는 상호 배타적)
	Source *ApplicationSource `json:"source,omitempty"`
	// 멀티소스 (v2.6+)
	Sources []ApplicationSource `json:"sources,omitempty"`
	// 배포 대상
	Destination ApplicationDestination `json:"destination"`
	// 속한 프로젝트
	Project string `json:"project"`
	// 자동 동기화 정책
	SyncPolicy *SyncPolicy `json:"syncPolicy,omitempty"`
	// 무시할 리소스 차이 (예: 자동으로 변경되는 필드)
	IgnoreDifferences []ResourceIgnoreDifferences `json:"ignoreDifferences,omitempty"`
}

// ResourceIgnoreDifferences는 diff 계산에서 무시할 필드를 지정한다.
type ResourceIgnoreDifferences struct {
	Group        string   `json:"group,omitempty"`
	Kind         string   `json:"kind"`
	Name         string   `json:"name,omitempty"`
	JSONPointers []string `json:"jsonPointers,omitempty"` // RFC 6901 JSON Pointer
}

// ResourceStatus는 App에 속한 개별 K8s 리소스의 상태다.
// kubectl get application myapp -o jsonpath='{.status.resources[*]}'
type ResourceStatus struct {
	Group     string           `json:"group,omitempty"`
	Version   string           `json:"version"`
	Kind      string           `json:"kind"`
	Namespace string           `json:"namespace,omitempty"`
	Name      string           `json:"name"`
	Status    SyncStatusCode   `json:"status,omitempty"`
	Health    *HealthStatus    `json:"health,omitempty"`
	Hook      bool             `json:"hook,omitempty"`
	RequiresPruning bool       `json:"requiresPruning,omitempty"`
}

// Operation은 대기 중인 동기화 작업이다.
// 소스: pkg/apis/application/v1alpha1/types.go — Operation
//
// 핵심 설계 포인트: 사용자가 UI/CLI에서 Sync 버튼을 누르면
// API Server가 Application.Operation 필드를 채운다.
// Application Controller가 이 필드를 감지해 sync를 수행하고,
// sync 완료 후 Operation 필드는 제거되며 OperationState에 결과가 저장된다.
type Operation struct {
	Sync *SyncOperation `json:"sync,omitempty"`
	// 작업을 시작한 사람 정보 (감사 로그)
	InitiatedBy OperationInitiator `json:"initiatedBy,omitempty"`
	// 동기화 전 정보 보존 (UI에서 표시용)
	Info []*Info `json:"info,omitempty"`
}

// SyncOperation은 Sync 작업의 세부 설정이다.
type SyncOperation struct {
	// Revision이 비어있으면 Spec.Source.TargetRevision 사용
	Revision  string   `json:"revision,omitempty"`
	Prune     bool     `json:"prune,omitempty"`
	DryRun    bool     `json:"dryRun,omitempty"`
	// 특정 리소스만 sync (비어있으면 전체)
	Resources []SyncOperationResource `json:"resources,omitempty"`
	SyncOptions []string `json:"syncOptions,omitempty"`
}

// SyncOperationResource는 개별 리소스 sync 대상이다.
type SyncOperationResource struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// OperationInitiator는 작업을 시작한 주체 정보다.
type OperationInitiator struct {
	Username  string `json:"username,omitempty"`
	Automated bool   `json:"automated,omitempty"` // 자동 sync인 경우 true
}

// Info는 임의 키-값 정보다.
type Info struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// OperationState는 완료된 작업의 결과다.
// Operation이 완료되면 Application.Operation은 제거되고
// Application.Status.OperationState에 저장된다.
type OperationState struct {
	Operation   Operation  `json:"operation"`
	Phase       string     `json:"phase"` // Running, Succeeded, Failed, Error
	Message     string     `json:"message,omitempty"`
	StartedAt   time.Time  `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	RetryCount  int64      `json:"retryCount,omitempty"`
}

// ApplicationStatus는 Application의 현재 상태다.
// 소스: pkg/apis/application/v1alpha1/types.go — ApplicationStatus
type ApplicationStatus struct {
	// 동기화 상태 (Git vs 클러스터 비교 결과)
	Sync SyncStatus `json:"sync,omitempty"`
	// 헬스 상태 (리소스들의 집계 상태)
	Health HealthStatus `json:"health,omitempty"`
	// 소속 리소스 목록
	Resources []ResourceStatus `json:"resources,omitempty"`
	// 완료된 작업 상태 (Operation 실행 결과)
	OperationState *OperationState `json:"operationState,omitempty"`
	// 마지막 갱신 시각
	ObservedAt time.Time `json:"observedAt,omitempty"`
	// 재조정 요청 마킹
	ReconciledAt *time.Time `json:"reconciledAt,omitempty"`
}

// Application은 Argo CD의 핵심 CRD다.
// 소스: pkg/apis/application/v1alpha1/types.go
//
//	type Application struct {
//	    metav1.TypeMeta   `json:",inline"`
//	    metav1.ObjectMeta `json:"metadata"`
//	    Spec              ApplicationSpec
//	    Status            ApplicationStatus
//	    Operation         *Operation
//	}
type Application struct {
	Kind      string `json:"kind"`
	Name      string `json:"metadata.name"`
	Namespace string `json:"metadata.namespace"`
	// 원하는 상태 (사용자가 정의, Git에 저장)
	Spec ApplicationSpec `json:"spec"`
	// 현재 상태 (컨트롤러가 업데이트, 읽기 전용)
	Status ApplicationStatus `json:"status,omitempty"`
	// 대기 중인 작업 (Sync 트리거)
	Operation *Operation `json:"operation,omitempty"`
}

// =============================================================================
// 4. AppProject CRD
//    소스: pkg/apis/application/v1alpha1/types.go — AppProject
// =============================================================================

// SyncWindow는 시간 기반 동기화 허용/차단 창이다.
// 소스: pkg/apis/application/v1alpha1/types.go — SyncWindow
type SyncWindow struct {
	// allow 또는 deny
	Kind string `json:"kind"`
	// cron 형식 (예: "0 22 * * *")
	Schedule string `json:"schedule"`
	// 창 지속 시간 (예: "1h")
	Duration string `json:"duration"`
	// 적용 대상
	Applications []string `json:"applications,omitempty"`
	Clusters     []string `json:"clusters,omitempty"`
	Namespaces   []string `json:"namespaces,omitempty"`
	// 수동 sync도 차단 여부
	ManualSync bool `json:"manualSync,omitempty"`
}

// ProjectRole은 프로젝트 단위 역할 정의다.
// 소스: pkg/apis/application/v1alpha1/types.go — ProjectRole
type ProjectRole struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	// Casbin 정책 규칙 (예: "p, proj:myapp:deploy, applications, sync, myapp/*, allow")
	Policies    []string     `json:"policies"`
	// JWT 토큰 (CI 시스템 등이 사람 계정 없이 사용)
	JWTTokens   []JWTToken   `json:"jwtTokens,omitempty"`
	Groups      []string     `json:"groups,omitempty"` // SSO 그룹 매핑
}

// JWTToken은 프로젝트 역할에 발급된 JWT 토큰 메타데이터다.
type JWTToken struct {
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp,omitempty"`
	ID        string `json:"id,omitempty"`
}

// AppProjectSpec은 프로젝트의 원하는 상태다.
type AppProjectSpec struct {
	// 허용된 소스 레포지토리 URL 패턴 (["*"]이면 전체 허용)
	SourceRepos []string `json:"sourceRepos"`
	// 허용된 배포 대상 (클러스터 + 네임스페이스 조합)
	Destinations []ApplicationDestination `json:"destinations"`
	// 프로젝트 설명
	Description string `json:"description,omitempty"`
	// 역할 정의
	Roles []ProjectRole `json:"roles,omitempty"`
	// 동기화 창
	SyncWindows []SyncWindow `json:"syncWindows,omitempty"`
	// 허용된 클러스터 리소스 종류 (namespace 수준)
	ClusterResourceWhitelist []GroupKind `json:"clusterResourceWhitelist,omitempty"`
	// 차단된 네임스페이스 리소스 종류
	NamespaceResourceBlacklist []GroupKind `json:"namespaceResourceBlacklist,omitempty"`
}

// GroupKind는 K8s 리소스의 API 그룹과 Kind다.
type GroupKind struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
}

// AppProject는 멀티테넌시 격리 단위 CRD다.
type AppProject struct {
	Kind string `json:"kind"`
	Name string `json:"metadata.name"`
	Spec AppProjectSpec `json:"spec"`
}

// =============================================================================
// 5. ApplicationSet CRD
//    소스: pkg/apis/application/v1alpha1/applicationset_types.go
// =============================================================================

// ApplicationSetGenerator는 Application 생성 시 사용할 데이터 소스다.
// 소스: pkg/apis/application/v1alpha1/applicationset_types.go — ApplicationSetGenerator
type ApplicationSetGenerator struct {
	// 정적 목록
	List *ListGenerator `json:"list,omitempty"`
	// 등록된 클러스터 목록
	Clusters *ClusterGenerator `json:"clusters,omitempty"`
	// Git 레포지토리의 디렉토리/파일 목록
	Git *GitGenerator `json:"git,omitempty"`
}

// ListGenerator는 정적으로 지정한 파라미터 목록으로 Application을 생성한다.
// 소스: applicationset/generators/list.go
type ListGenerator struct {
	// 각 항목이 하나의 Application 템플릿 파라미터 셋
	Elements []map[string]string `json:"elements"`
}

// ClusterGenerator는 등록된 클러스터마다 Application을 생성한다.
// 소스: applicationset/generators/cluster.go
type ClusterGenerator struct {
	// 클러스터 레이블 셀렉터
	Selector map[string]string `json:"selector,omitempty"`
	// 클러스터 Secret에서 추가 값 추출
	Values map[string]string `json:"values,omitempty"`
}

// GitGenerator는 Git 레포지토리 디렉토리/파일 목록으로 Application을 생성한다.
type GitGenerator struct {
	RepoURL   string            `json:"repoURL"`
	Revision  string            `json:"revision,omitempty"`
	Directories []GitDirectoryGeneratorItem `json:"directories,omitempty"`
}

// GitDirectoryGeneratorItem은 Git 디렉토리 패턴이다.
type GitDirectoryGeneratorItem struct {
	Path    string `json:"path"`
	Exclude bool   `json:"exclude,omitempty"`
}

// ApplicationSetTemplate은 생성할 Application의 템플릿이다.
// {{ .cluster }} 같은 Go template 문법을 사용한다.
type ApplicationSetTemplate struct {
	// 템플릿에서 생성될 Application의 메타데이터
	Name      string `json:"metadata.name"` // 예: "{{.cluster}}-myapp"
	Namespace string `json:"metadata.namespace,omitempty"`
	Spec ApplicationSpec `json:"spec"`
}

// ApplicationSetSpec은 ApplicationSet의 원하는 상태다.
type ApplicationSetSpec struct {
	Generators []ApplicationSetGenerator `json:"generators"`
	Template   ApplicationSetTemplate   `json:"template"`
	// 생성된 App이 삭제될 때 클러스터 리소스도 삭제할지 여부
	SyncPolicy *ApplicationSetSyncPolicy `json:"syncPolicy,omitempty"`
}

// ApplicationSetSyncPolicy는 ApplicationSet 수준 sync 정책이다.
type ApplicationSetSyncPolicy struct {
	PreserveResourcesOnDeletion bool `json:"preserveResourcesOnDeletion,omitempty"`
}

// ApplicationSet은 여러 Application을 자동으로 생성/관리하는 CRD다.
type ApplicationSet struct {
	Kind string `json:"kind"`
	Name string `json:"metadata.name"`
	Spec ApplicationSetSpec `json:"spec"`
}

// =============================================================================
// 6. Cluster 및 Repository 타입
// =============================================================================

// Cluster는 Argo CD가 관리하는 대상 K8s 클러스터를 나타낸다.
// 실제로는 K8s Secret에 저장되며, 이 구조체는 런타임 표현이다.
// 소스: pkg/apis/application/v1alpha1/types.go — Cluster
type Cluster struct {
	// K8s API Server URL
	Server string `json:"server"`
	// 표시 이름
	Name string `json:"name"`
	// 연결 설정
	Config ClusterConfig `json:"config"`
	// 클러스터 연결 상태
	ConnectionState ConnectionState `json:"connectionState,omitempty"`
}

// ClusterConfig는 K8s 클러스터 연결 인증 정보다.
type ClusterConfig struct {
	// ServiceAccount Bearer 토큰
	BearerToken string `json:"bearerToken,omitempty"`
	// TLS 설정
	TLSClientConfig TLSClientConfig `json:"tlsClientConfig,omitempty"`
}

// TLSClientConfig는 TLS 연결 설정이다.
type TLSClientConfig struct {
	Insecure bool   `json:"insecure,omitempty"`
	CertData []byte `json:"certData,omitempty"`
	KeyData  []byte `json:"keyData,omitempty"`
	CAData   []byte `json:"caData,omitempty"`
}

// ConnectionState는 클러스터 연결 상태다.
type ConnectionState struct {
	Status     string    `json:"status"` // Successful, Failed, Unknown
	Message    string    `json:"message,omitempty"`
	ModifiedAt time.Time `json:"attemptedAt"`
}

// Repository는 Git/Helm 레포지토리 인증 정보다.
// 소스: pkg/apis/application/v1alpha1/repository_types.go
type Repository struct {
	Repo     string `json:"repo"`     // 레포지토리 URL
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"` // 암호화됨
	SSHPrivateKey string `json:"sshPrivateKey,omitempty"`
	// 레포지토리 타입: git | helm
	Type string `json:"type,omitempty"`
	// Helm 레포지토리 이름 (Helm repo용)
	Name string `json:"name,omitempty"`
}

// =============================================================================
// 7. 데이터 생명주기 시뮬레이션
//    흐름: App 생성 → OutOfSync → Sync → Healthy
// =============================================================================

func demonstrateLifecycle() {
	factor := int64(2)
	maxDuration := "3m"

	// --- App 생성 ---
	app := &Application{
		Kind:      "Application",
		Name:      "myapp",
		Namespace: "argocd",
		Spec: ApplicationSpec{
			Source: &ApplicationSource{
				RepoURL:        "https://github.com/myorg/myapp.git",
				Path:           "helm/myapp",
				TargetRevision: "HEAD",
				Helm: &ApplicationSourceHelm{
					ReleaseName: "myapp",
					ValueFiles:  []string{"values-prod.yaml"},
					Parameters: []HelmParameter{
						{Name: "image.tag", Value: "v1.2.3"},
						{Name: "replicaCount", Value: "3"},
					},
				},
			},
			Destination: ApplicationDestination{
				Server:    "https://production.k8s.local:6443",
				Namespace: "production",
			},
			Project: "myproject",
			SyncPolicy: &SyncPolicy{
				Automated: &SyncPolicyAutomated{
					Prune:    true,
					SelfHeal: true,
				},
				SyncOptions: []string{"CreateNamespace=true", "ServerSideApply=true"},
				Retry: &RetryStrategy{
					Limit: 5,
					Backoff: &Backoff{
						Duration:    "5s",
						Factor:      &factor,
						MaxDuration: maxDuration,
					},
				},
			},
			IgnoreDifferences: []ResourceIgnoreDifferences{
				{
					Kind:         "Deployment",
					JSONPointers: []string{"/spec/replicas"}, // HPA가 관리
				},
			},
		},
		Status: ApplicationStatus{
			Sync:   SyncStatus{Status: SyncStatusCodeUnknown},
			Health: HealthStatus{Status: HealthStatusUnknown},
		},
	}

	fmt.Println("=================================================================")
	fmt.Println(" Argo CD 핵심 데이터 모델 시뮬레이션")
	fmt.Println("=================================================================")
	fmt.Println()

	// --- 단계 1: 초기 상태 ---
	fmt.Printf("[단계 1] Application 생성\n")
	fmt.Printf("  name:      %s\n", app.Name)
	fmt.Printf("  project:   %s\n", app.Spec.Project)
	fmt.Printf("  repoURL:   %s\n", app.Spec.Source.RepoURL)
	fmt.Printf("  path:      %s\n", app.Spec.Source.Path)
	fmt.Printf("  revision:  %s\n", app.Spec.Source.TargetRevision)
	fmt.Printf("  dest:      %s/%s\n", app.Spec.Destination.Server, app.Spec.Destination.Namespace)
	fmt.Printf("  autoSync:  prune=%v, selfHeal=%v\n",
		app.Spec.SyncPolicy.Automated.Prune,
		app.Spec.SyncPolicy.Automated.SelfHeal)
	fmt.Printf("  syncStatus: %s, health: %s\n", app.Status.Sync.Status, app.Status.Health.Status)
	fmt.Println()

	// --- 단계 2: Controller가 Git과 비교 → OutOfSync 감지 ---
	fmt.Printf("[단계 2] Controller: CompareAppState 실행 → OutOfSync 감지\n")
	now := time.Now()
	app.Status.Sync = SyncStatus{
		Status:   SyncStatusCodeOutOfSync,
		Revision: "a1b2c3d4e5f6",
	}
	app.Status.Resources = []ResourceStatus{
		{
			Version: "apps/v1", Kind: "Deployment", Name: "myapp",
			Namespace: "production",
			Status:    SyncStatusCodeOutOfSync,
			Health:    &HealthStatus{Status: HealthStatusHealthy},
		},
		{
			Version: "v1", Kind: "Service", Name: "myapp",
			Namespace: "production",
			Status:    SyncStatusCodeSynced,
			Health:    &HealthStatus{Status: HealthStatusHealthy},
		},
		{
			Version: "v1", Kind: "ConfigMap", Name: "myapp-config",
			Namespace: "production",
			Status:    SyncStatusCodeOutOfSync,
			Health:    &HealthStatus{Status: HealthStatusHealthy},
		},
	}
	app.Status.ObservedAt = now
	reconciledAt := now
	app.Status.ReconciledAt = &reconciledAt

	fmt.Printf("  syncStatus: %s (revision: %s)\n",
		app.Status.Sync.Status, app.Status.Sync.Revision)
	for _, r := range app.Status.Resources {
		fmt.Printf("    %-12s %-20s %s\n", r.Kind, r.Name, r.Status)
	}
	fmt.Println()

	// --- 단계 3: autoSync → Operation 설정 ---
	fmt.Printf("[단계 3] autoSync: Application.Operation 필드 설정 (Sync 트리거)\n")
	app.Operation = &Operation{
		Sync: &SyncOperation{
			Revision: "a1b2c3d4e5f6",
			Prune:    true,
		},
		InitiatedBy: OperationInitiator{
			Automated: true, // 자동 sync
		},
		Info: []*Info{
			{Name: "reason", Value: "git diff detected"},
		},
	}
	fmt.Printf("  Operation.Sync.Revision: %s\n", app.Operation.Sync.Revision)
	fmt.Printf("  Operation.InitiatedBy.Automated: %v\n", app.Operation.InitiatedBy.Automated)
	fmt.Println()

	// --- 단계 4: Sync 실행 ---
	fmt.Printf("[단계 4] Controller: Sync 실행 중 (apply resources)\n")
	app.Status.Health = HealthStatus{
		Status:  HealthStatusProgressing,
		Message: "Waiting for Deployment rollout",
	}
	startedAt := time.Now()
	opState := &OperationState{
		Operation: *app.Operation,
		Phase:     "Running",
		StartedAt: startedAt,
	}
	app.Status.OperationState = opState
	app.Operation = nil // 작업 시작 후 Operation 필드 제거
	fmt.Printf("  health: %s (%s)\n", app.Status.Health.Status, app.Status.Health.Message)
	fmt.Printf("  operationState.phase: %s\n", app.Status.OperationState.Phase)
	fmt.Printf("  Operation 필드: %v (제거됨)\n", app.Operation)
	fmt.Println()

	// --- 단계 5: Sync 완료 → Healthy ---
	fmt.Printf("[단계 5] Sync 완료 → Healthy\n")
	finishedAt := time.Now()
	app.Status.OperationState.Phase = "Succeeded"
	app.Status.OperationState.FinishedAt = &finishedAt
	app.Status.Sync = SyncStatus{
		Status:   SyncStatusCodeSynced,
		Revision: "a1b2c3d4e5f6",
	}
	// 모든 리소스 상태 갱신
	for i := range app.Status.Resources {
		app.Status.Resources[i].Status = SyncStatusCodeSynced
	}
	// 집계 헬스 상태 계산
	app.Status.Health = aggregateHealth(app.Status.Resources)
	fmt.Printf("  syncStatus: %s\n", app.Status.Sync.Status)
	fmt.Printf("  health: %s\n", app.Status.Health.Status)
	fmt.Printf("  operationState.phase: %s\n", app.Status.OperationState.Phase)
	fmt.Printf("  소요 시간: %v\n", app.Status.OperationState.FinishedAt.Sub(app.Status.OperationState.StartedAt).Round(time.Millisecond))
	fmt.Println()
}

// aggregateHealth는 리소스 목록에서 전체 헬스를 집계한다.
// 소스: util/health/health.go — GetApplicationHealth()
// 가장 나쁜 리소스의 상태가 전체 App 헬스가 된다.
func aggregateHealth(resources []ResourceStatus) HealthStatus {
	worst := HealthStatusHealthy
	for _, r := range resources {
		if r.Health != nil && IsWorse(r.Health.Status, worst) {
			worst = r.Health.Status
		}
	}
	return HealthStatus{Status: worst}
}

// =============================================================================
// 8. AppProject 시뮬레이션
// =============================================================================

func demonstrateAppProject() {
	fmt.Println("=================================================================")
	fmt.Println(" AppProject 데이터 모델")
	fmt.Println("=================================================================")

	project := &AppProject{
		Kind: "AppProject",
		Name: "myproject",
		Spec: AppProjectSpec{
			Description: "Production environment applications",
			SourceRepos: []string{
				"https://github.com/myorg/*",
				"https://charts.helm.sh/stable",
			},
			Destinations: []ApplicationDestination{
				{Server: "https://production.k8s.local:6443", Namespace: "production"},
				{Server: "https://production.k8s.local:6443", Namespace: "monitoring"},
			},
			Roles: []ProjectRole{
				{
					Name:        "developer",
					Description: "개발자: sync 권한만 있음",
					Policies: []string{
						"p, proj:myproject:developer, applications, get, myproject/*, allow",
						"p, proj:myproject:developer, applications, sync, myproject/*, allow",
					},
					Groups: []string{"myorg:developers"},
				},
				{
					Name:        "admin",
					Description: "관리자: 전체 권한",
					Policies: []string{
						"p, proj:myproject:admin, applications, *, myproject/*, allow",
					},
					Groups: []string{"myorg:platform-team"},
				},
			},
			SyncWindows: []SyncWindow{
				{
					Kind:         "allow",
					Schedule:     "0 9 * * 1-5", // 평일 09:00
					Duration:     "8h",
					Applications: []string{"*"},
				},
				{
					Kind:         "deny",
					Schedule:     "0 20 * * *", // 매일 20:00
					Duration:     "12h",         // 자정까지 배포 금지
					Applications: []string{"*"},
					ManualSync:   true,
				},
			},
			ClusterResourceWhitelist: []GroupKind{
				{Group: "", Kind: "Namespace"},
				{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"},
			},
		},
	}

	fmt.Printf("\n  Project: %s\n", project.Name)
	fmt.Printf("  허용된 소스 레포:\n")
	for _, repo := range project.Spec.SourceRepos {
		fmt.Printf("    - %s\n", repo)
	}
	fmt.Printf("  허용된 배포 대상:\n")
	for _, dest := range project.Spec.Destinations {
		fmt.Printf("    - %s / %s\n", dest.Server, dest.Namespace)
	}
	fmt.Printf("  역할 수: %d\n", len(project.Spec.Roles))
	for _, role := range project.Spec.Roles {
		fmt.Printf("    - %s: %d개 정책, 그룹=%v\n", role.Name, len(role.Policies), role.Groups)
	}
	fmt.Printf("  동기화 창:\n")
	for _, w := range project.Spec.SyncWindows {
		fmt.Printf("    - %s | schedule=%q | duration=%s | manualSync=%v\n",
			w.Kind, w.Schedule, w.Duration, w.ManualSync)
	}
	fmt.Println()
}

// =============================================================================
// 9. ApplicationSet 시뮬레이션
// =============================================================================

func demonstrateApplicationSet() {
	fmt.Println("=================================================================")
	fmt.Println(" ApplicationSet 데이터 모델")
	fmt.Println("=================================================================")

	appset := &ApplicationSet{
		Kind: "ApplicationSet",
		Name: "myapp-per-cluster",
		Spec: ApplicationSetSpec{
			Generators: []ApplicationSetGenerator{
				{
					// List 제너레이터: 정적으로 정의된 환경 목록
					List: &ListGenerator{
						Elements: []map[string]string{
							{"cluster": "staging", "url": "https://staging.k8s.local:6443", "env": "staging"},
							{"cluster": "production", "url": "https://production.k8s.local:6443", "env": "production"},
							{"cluster": "dr", "url": "https://dr.k8s.local:6443", "env": "production"},
						},
					},
				},
			},
			Template: ApplicationSetTemplate{
				Name:      "{{.cluster}}-myapp", // Go template
				Namespace: "argocd",
				Spec: ApplicationSpec{
					Source: &ApplicationSource{
						RepoURL:        "https://github.com/myorg/myapp.git",
						Path:           "helm/myapp",
						TargetRevision: "HEAD",
						Helm: &ApplicationSourceHelm{
							ValueFiles: []string{"values-{{.env}}.yaml"},
						},
					},
					Destination: ApplicationDestination{
						Server:    "{{.url}}",
						Namespace: "myapp",
					},
					Project: "myproject",
				},
			},
		},
	}

	fmt.Printf("\n  ApplicationSet: %s\n", appset.Name)
	fmt.Printf("  제너레이터 타입: List\n")

	// 제너레이터가 생성할 Application 목록 시뮬레이션
	fmt.Printf("\n  생성될 Application 목록:\n")
	elements := appset.Spec.Generators[0].List.Elements
	for _, elem := range elements {
		appName := strings.ReplaceAll(appset.Spec.Template.Name, "{{.cluster}}", elem["cluster"])
		destServer := strings.ReplaceAll(appset.Spec.Template.Spec.Destination.Server, "{{.url}}", elem["url"])
		valueFile := strings.ReplaceAll(appset.Spec.Template.Spec.Source.Helm.ValueFiles[0], "{{.env}}", elem["env"])
		fmt.Printf("    Application: %-25s → %s (values: %s)\n", appName, destServer, valueFile)
	}
	fmt.Println()
}

// =============================================================================
// 10. Health 모델 시연
// =============================================================================

func demonstrateHealthModel() {
	fmt.Println("=================================================================")
	fmt.Println(" Health 모델 — IsWorse() 함수")
	fmt.Println("=================================================================")
	fmt.Println()

	// 심각도 순서 출력
	fmt.Println("  헬스 상태 심각도 순서 (낮을수록 좋음):")
	orderedStatuses := []HealthStatusCode{
		HealthStatusHealthy,
		HealthStatusSuspended,
		HealthStatusProgressing,
		HealthStatusMissing,
		HealthStatusDegraded,
		HealthStatusUnknown,
	}
	for i, s := range orderedStatuses {
		fmt.Printf("    %d. %-15s (order: %d)\n", i+1, s, healthOrder[s])
	}
	fmt.Println()

	// IsWorse() 예시
	fmt.Println("  IsWorse() 비교 예시:")
	comparisons := [][2]HealthStatusCode{
		{HealthStatusDegraded, HealthStatusHealthy},
		{HealthStatusHealthy, HealthStatusDegraded},
		{HealthStatusProgressing, HealthStatusSuspended},
		{HealthStatusMissing, HealthStatusProgressing},
	}
	for _, pair := range comparisons {
		result := IsWorse(pair[0], pair[1])
		fmt.Printf("    IsWorse(%-15s, %-15s) = %v\n", pair[0], pair[1], result)
	}
	fmt.Println()

	// 리소스 집계 헬스 예시
	fmt.Println("  리소스 집계 헬스 (App 전체 헬스 = 가장 나쁜 리소스 상태):")
	resourceSets := [][]ResourceStatus{
		{
			{Kind: "Deployment", Name: "api", Health: &HealthStatus{Status: HealthStatusHealthy}},
			{Kind: "Deployment", Name: "worker", Health: &HealthStatus{Status: HealthStatusProgressing}},
			{Kind: "Service", Name: "api-svc", Health: &HealthStatus{Status: HealthStatusHealthy}},
		},
		{
			{Kind: "Deployment", Name: "api", Health: &HealthStatus{Status: HealthStatusHealthy}},
			{Kind: "StatefulSet", Name: "db", Health: &HealthStatus{Status: HealthStatusDegraded}},
		},
		{
			{Kind: "Deployment", Name: "api", Health: &HealthStatus{Status: HealthStatusHealthy}},
			{Kind: "Service", Name: "api-svc", Health: &HealthStatus{Status: HealthStatusHealthy}},
		},
	}
	for _, resources := range resourceSets {
		agg := aggregateHealth(resources)
		names := make([]string, len(resources))
		for i, r := range resources {
			names[i] = fmt.Sprintf("%s(%s)", r.Kind, r.Health.Status)
		}
		fmt.Printf("    [%s]\n    → 집계 헬스: %s\n\n",
			strings.Join(names, ", "), agg.Status)
	}
}

func main() {
	demonstrateLifecycle()
	demonstrateAppProject()
	demonstrateApplicationSet()
	demonstrateHealthModel()
}
