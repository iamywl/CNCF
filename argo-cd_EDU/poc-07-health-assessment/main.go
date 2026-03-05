// poc-07-health-assessment: Argo CD 헬스 평가 엔진 시뮬레이션
//
// 실제 소스 참조:
//   - gitops-engine/pkg/health/health.go: HealthStatusCode, healthOrder, IsWorse(), GetResourceHealth()
//   - gitops-engine/pkg/health/health.go:44-52: healthOrder 배열
//   - gitops-engine/pkg/health/health.go:54-67: IsWorse() 함수
//   - gitops-engine/pkg/health/health.go:70-101: GetResourceHealth() 우선순위
//   - gitops-engine/pkg/health/health_deployment.go: getAppsv1DeploymentHealth()
//   - gitops-engine/pkg/health/health_pod.go: getCorev1PodHealth()
//   - gitops-engine/pkg/health/health_pvc.go: getCorev1PVCHealth()
//   - gitops-engine/pkg/health/health_job.go: getBatchv1JobHealth()
//   - gitops-engine/pkg/health/health_statefulset.go: getAppsv1StatefulSetHealth()
//   - controller/health.go: setApplicationHealth() — 앱 레벨 헬스 집계
//
// go run main.go
package main

import (
	"fmt"
	"strings"
	"time"
)

// ─────────────────────────────────────────────
// HealthStatusCode — 헬스 상태 코드
// 실제: gitops-engine/pkg/health/health.go:14-31
// ─────────────────────────────────────────────

type HealthStatusCode string

const (
	// HealthStatusUnknown: 헬스 평가 실패, 실제 상태 불명
	HealthStatusUnknown HealthStatusCode = "Unknown"
	// HealthStatusProgressing: 아직 Healthy가 아니지만 도달 가능한 상태로 진행 중
	HealthStatusProgressing HealthStatusCode = "Progressing"
	// HealthStatusHealthy: 100% 건강
	HealthStatusHealthy HealthStatusCode = "Healthy"
	// HealthStatusSuspended: 일시 중단됨 (예: CronJob suspend)
	HealthStatusSuspended HealthStatusCode = "Suspended"
	// HealthStatusDegraded: 실패 상태 또는 일정 시간 내 Healthy 불가
	HealthStatusDegraded HealthStatusCode = "Degraded"
	// HealthStatusMissing: 클러스터에 리소스 없음
	HealthStatusMissing HealthStatusCode = "Missing"
)

// healthOrder는 가장 건강한 상태에서 가장 나쁜 상태 순서
// 실제: gitops-engine/pkg/health/health.go:44-52
var healthOrder = []HealthStatusCode{
	HealthStatusHealthy,
	HealthStatusSuspended,
	HealthStatusProgressing,
	HealthStatusMissing,
	HealthStatusDegraded,
	HealthStatusUnknown,
}

// HealthStatus는 헬스 평가 결과
// 실제: gitops-engine/pkg/health/health.go:38-42
type HealthStatus struct {
	Status  HealthStatusCode
	Message string
}

func (h *HealthStatus) String() string {
	if h.Message != "" {
		return fmt.Sprintf("%s (%s)", h.Status, h.Message)
	}
	return string(h.Status)
}

// IsWorse는 new 상태가 current보다 나쁜지 반환한다.
// 실제: gitops-engine/pkg/health/health.go:54-67
// 앱 전체 헬스 = 모든 리소스 중 가장 나쁜 상태
func IsWorse(current, new HealthStatusCode) bool {
	currentIndex := 0
	newIndex := 0
	for i, code := range healthOrder {
		if current == code {
			currentIndex = i
		}
		if new == code {
			newIndex = i
		}
	}
	return newIndex > currentIndex
}

// ─────────────────────────────────────────────
// 리소스 표현 (간소화된 Kubernetes 리소스)
// ─────────────────────────────────────────────

// GVK (Group, Version, Kind)
type GVK struct {
	Group   string
	Version string
	Kind    string
}

// Resource는 쿠버네티스 리소스를 나타낸다
type Resource struct {
	GVK
	Name              string
	Namespace         string
	DeletionTimestamp *time.Time // DeletionTimestamp != nil → Progressing
	Annotations       map[string]string

	// 리소스별 상태 필드
	Spec   ResourceSpec
	Status ResourceStatus
}

type ResourceSpec struct {
	Replicas  *int32
	Paused    bool
	PVCPhase  string // Pending, Bound, Lost
	JobSpec   JobSpec
}

type ResourceStatus struct {
	Phase             string
	AvailableReplicas int32
	UpdatedReplicas   int32
	ReadyReplicas     int32
	ObservedGeneration int64
	CurrentReplicas   int32
	Generation        int64
	CurrentRevision   string
	UpdateRevision    string
	Conditions        []Condition
	PodPhase          string
	RestartPolicy     string
	ContainerStatuses []ContainerStatus
}

type Condition struct {
	Type    string
	Status  string // True/False
	Reason  string
	Message string
}

type ContainerStatus struct {
	Name                 string
	WaitingReason        string
	WaitingMessage       string
	LastTerminated       bool
}

type JobSpec struct {
	Completions *int32
}

// ─────────────────────────────────────────────
// HealthOverride — Lua 스크립트 대체 시뮬레이션
// 실제: gitops-engine/pkg/health/health.go:33-36
// ─────────────────────────────────────────────

// HealthOverride는 내장 헬스 체크를 재정의하는 인터페이스
type HealthOverride interface {
	GetResourceHealth(r *Resource) (*HealthStatus, error)
}

// LuaHealthOverride는 사용자 정의 Lua 스크립트 기반 헬스 체크를 시뮬레이션한다
// 실제로는 util/lua 패키지를 통해 Lua VM에서 실행됨
type LuaHealthOverride struct {
	// scriptName은 어떤 Lua 스크립트를 사용하는지 (예: "custom-CRD-health-check")
	ScriptName string
	// OverrideFunc는 Lua 스크립트 실행 결과를 시뮬레이션
	OverrideFunc func(r *Resource) (*HealthStatus, error)
}

func (l *LuaHealthOverride) GetResourceHealth(r *Resource) (*HealthStatus, error) {
	return l.OverrideFunc(r)
}

// ─────────────────────────────────────────────
// GetResourceHealth — 헬스 평가 우선순위
// 실제: gitops-engine/pkg/health/health.go:70-101
// ─────────────────────────────────────────────

// GetResourceHealth는 리소스의 헬스를 평가한다.
// 우선순위:
//  1. DeletionTimestamp != nil → Progressing (삭제 진행 중)
//  2. healthOverride (Lua 스크립트) → override 결과 반환
//  3. GVK별 내장 헬스 체크
//  4. nil → 헬스 개념 없음 (ConfigMap, Secret 등)
func GetResourceHealth(r *Resource, healthOverride HealthOverride) (*HealthStatus, error) {
	// 1. DeletionTimestamp 확인
	// 실제: gitops-engine/pkg/health/health.go:71-76
	if r.DeletionTimestamp != nil {
		return &HealthStatus{
			Status:  HealthStatusProgressing,
			Message: "Pending deletion",
		}, nil
	}

	// 2. healthOverride (Lua 스크립트)
	// 실제: gitops-engine/pkg/health/health.go:78-90
	if healthOverride != nil {
		health, err := healthOverride.GetResourceHealth(r)
		if err != nil {
			return &HealthStatus{
				Status:  HealthStatusUnknown,
				Message: err.Error(),
			}, err
		}
		if health != nil {
			return health, nil
		}
		// nil 반환 시 내장 체크로 fallback
	}

	// 3. GVK별 내장 헬스 체크
	// 실제: gitops-engine/pkg/health/health.go:92-99
	return getBuiltinHealth(r)
}

// getBuiltinHealth는 GVK에 따라 내장 헬스 체크를 실행한다
// 실제: gitops-engine/pkg/health/health.go:103-152 GetHealthCheckFunc()
func getBuiltinHealth(r *Resource) (*HealthStatus, error) {
	switch r.Group {
	case "apps":
		switch r.Kind {
		case "Deployment":
			return getDeploymentHealth(r)
		case "StatefulSet":
			return getStatefulSetHealth(r)
		}
	case "":
		switch r.Kind {
		case "Pod":
			return getPodHealth(r)
		case "PersistentVolumeClaim":
			return getPVCHealth(r)
		case "Service":
			return getServiceHealth(r)
		}
	case "batch":
		if r.Kind == "Job" {
			return getJobHealth(r)
		}
	}
	// 4. 해당 없음 → nil (ConfigMap, Secret 등은 헬스 개념 없음)
	return nil, nil
}

// ─────────────────────────────────────────────
// Deployment 헬스 체크
// 실제: gitops-engine/pkg/health/health_deployment.go:28-70
// getAppsv1DeploymentHealth()
// ─────────────────────────────────────────────

func getDeploymentHealth(r *Resource) (*HealthStatus, error) {
	// Paused → Suspended
	if r.Spec.Paused {
		return &HealthStatus{
			Status:  HealthStatusSuspended,
			Message: "Deployment is paused",
		}, nil
	}

	// ObservedGeneration 확인
	// 실제: "if deployment.Generation <= deployment.Status.ObservedGeneration"
	if r.Status.Generation > r.Status.ObservedGeneration {
		return &HealthStatus{
			Status:  HealthStatusProgressing,
			Message: "Waiting for rollout to finish: observed deployment generation less than desired generation",
		}, nil
	}

	// Conditions 확인: ProgressDeadlineExceeded
	for _, cond := range r.Status.Conditions {
		if cond.Type == "Progressing" && cond.Reason == "ProgressDeadlineExceeded" {
			return &HealthStatus{
				Status:  HealthStatusDegraded,
				Message: fmt.Sprintf("Deployment %q exceeded its progress deadline", r.Name),
			}, nil
		}
	}

	replicas := int32(1)
	if r.Spec.Replicas != nil {
		replicas = *r.Spec.Replicas
	}

	// UpdatedReplicas < replicas
	if r.Status.UpdatedReplicas < replicas {
		return &HealthStatus{
			Status: HealthStatusProgressing,
			Message: fmt.Sprintf("Waiting for rollout to finish: %d out of %d new replicas have been updated...",
				r.Status.UpdatedReplicas, replicas),
		}, nil
	}

	// AvailableReplicas < UpdatedReplicas
	if r.Status.AvailableReplicas < r.Status.UpdatedReplicas {
		return &HealthStatus{
			Status: HealthStatusProgressing,
			Message: fmt.Sprintf("Waiting for rollout to finish: %d of %d updated replicas are available...",
				r.Status.AvailableReplicas, r.Status.UpdatedReplicas),
		}, nil
	}

	return &HealthStatus{Status: HealthStatusHealthy}, nil
}

// ─────────────────────────────────────────────
// StatefulSet 헬스 체크
// 실제: gitops-engine/pkg/health/health_statefulset.go:28-73
// getAppsv1StatefulSetHealth()
// ─────────────────────────────────────────────

func getStatefulSetHealth(r *Resource) (*HealthStatus, error) {
	// ObservedGeneration 확인
	// 실제: "if sts.Status.ObservedGeneration == 0 || sts.Generation > sts.Status.ObservedGeneration"
	if r.Status.ObservedGeneration == 0 || r.Status.Generation > r.Status.ObservedGeneration {
		return &HealthStatus{
			Status:  HealthStatusProgressing,
			Message: "Waiting for statefulset spec update to be observed...",
		}, nil
	}

	replicas := int32(1)
	if r.Spec.Replicas != nil {
		replicas = *r.Spec.Replicas
	}

	// ReadyReplicas < replicas
	if r.Status.ReadyReplicas < replicas {
		return &HealthStatus{
			Status: HealthStatusProgressing,
			Message: fmt.Sprintf("Waiting for %d pods to be ready...",
				replicas-r.Status.ReadyReplicas),
		}, nil
	}

	// UpdateRevision != CurrentRevision (롤링 업데이트 중)
	if r.Status.UpdateRevision != r.Status.CurrentRevision {
		return &HealthStatus{
			Status: HealthStatusProgressing,
			Message: fmt.Sprintf("waiting for statefulset rolling update to complete %d pods at revision %s...",
				r.Status.UpdatedReplicas, r.Status.UpdateRevision),
		}, nil
	}

	return &HealthStatus{
		Status: HealthStatusHealthy,
		Message: fmt.Sprintf("statefulset rolling update complete %d pods at revision %s...",
			r.Status.CurrentReplicas, r.Status.CurrentRevision),
	}, nil
}

// ─────────────────────────────────────────────
// Pod 헬스 체크
// 실제: gitops-engine/pkg/health/health_pod.go:30-134
// getCorev1PodHealth()
// ─────────────────────────────────────────────

func getPodHealth(r *Resource) (*HealthStatus, error) {
	// RestartPolicy=Always인 경우: 컨테이너 오류 상태 먼저 확인
	// 실제: "if pod.Spec.RestartPolicy == corev1.RestartPolicyAlways"
	if r.Status.RestartPolicy == "Always" {
		for _, cs := range r.Status.ContainerStatuses {
			waiting := cs.WaitingReason
			// ErrImagePull, ImagePullBackOff, CrashLoopBackOff 등
			if strings.HasPrefix(waiting, "Err") ||
				strings.HasSuffix(waiting, "Error") ||
				strings.HasSuffix(waiting, "BackOff") {
				return &HealthStatus{
					Status:  HealthStatusDegraded,
					Message: cs.WaitingMessage,
				}, nil
			}
		}
	}

	// Phase 기반 판단
	switch r.Status.PodPhase {
	case "Pending":
		return &HealthStatus{Status: HealthStatusProgressing}, nil
	case "Succeeded":
		return &HealthStatus{Status: HealthStatusHealthy}, nil
	case "Failed":
		return &HealthStatus{Status: HealthStatusDegraded, Message: "pod failed"}, nil
	case "Running":
		switch r.Status.RestartPolicy {
		case "Always":
			// Pod Ready 여부 확인
			// 실제: podutils.IsPodReady(pod)
			ready := true
			for _, cs := range r.Status.ContainerStatuses {
				if cs.LastTerminated {
					return &HealthStatus{
						Status:  HealthStatusDegraded,
						Message: "container has terminated previously",
					}, nil
				}
				if cs.WaitingReason != "" {
					ready = false
				}
			}
			if ready {
				return &HealthStatus{Status: HealthStatusHealthy}, nil
			}
			return &HealthStatus{Status: HealthStatusProgressing}, nil
		case "OnFailure", "Never":
			// Hook Pod는 Progressing으로 취급 (일시적인 리소스)
			return &HealthStatus{Status: HealthStatusProgressing}, nil
		}
	}
	return &HealthStatus{Status: HealthStatusUnknown}, nil
}

// ─────────────────────────────────────────────
// PVC 헬스 체크
// 실제: gitops-engine/pkg/health/health_pvc.go:28-41
// getCorev1PVCHealth()
// ─────────────────────────────────────────────

func getPVCHealth(r *Resource) (*HealthStatus, error) {
	var status HealthStatusCode
	switch r.Spec.PVCPhase {
	case "Lost":
		status = HealthStatusDegraded
	case "Pending":
		status = HealthStatusProgressing
	case "Bound":
		status = HealthStatusHealthy
	default:
		status = HealthStatusUnknown
	}
	return &HealthStatus{Status: status}, nil
}

// ─────────────────────────────────────────────
// Service 헬스 체크
// 실제: gitops-engine/pkg/health/health_service.go
// ─────────────────────────────────────────────

func getServiceHealth(r *Resource) (*HealthStatus, error) {
	// LoadBalancer 타입은 외부 IP 할당 대기 가능하나
	// 기본적으로 Service는 생성되면 Healthy
	return &HealthStatus{Status: HealthStatusHealthy}, nil
}

// ─────────────────────────────────────────────
// Job 헬스 체크
// 실제: gitops-engine/pkg/health/health_job.go:30-75
// getBatchv1JobHealth()
// ─────────────────────────────────────────────

func getJobHealth(r *Resource) (*HealthStatus, error) {
	failed := false
	complete := false
	isSuspended := false
	var failMsg, message string

	for _, cond := range r.Status.Conditions {
		switch cond.Type {
		case "Failed":
			failed = true
			complete = true
			failMsg = cond.Message
		case "Complete":
			complete = true
			message = cond.Message
		case "Suspended":
			complete = true
			message = cond.Message
			if cond.Status == "True" {
				isSuspended = true
			}
		}
	}

	switch {
	case !complete:
		return &HealthStatus{Status: HealthStatusProgressing, Message: message}, nil
	case failed:
		return &HealthStatus{Status: HealthStatusDegraded, Message: failMsg}, nil
	case isSuspended:
		return &HealthStatus{Status: HealthStatusSuspended, Message: message}, nil
	default:
		return &HealthStatus{Status: HealthStatusHealthy, Message: message}, nil
	}
}

// ─────────────────────────────────────────────
// 앱 레벨 헬스 집계
// 실제: controller/health.go:setApplicationHealth()
// 모든 관리 리소스 중 "가장 나쁜" 상태가 앱 헬스
// ─────────────────────────────────────────────

type ManagedResource struct {
	Resource       *Resource
	HealthOverride HealthOverride
	IsHook         bool // hook은 헬스 집계에서 제외
}

// AssessApplicationHealth는 모든 리소스의 헬스를 평가하고 앱 전체 헬스를 반환한다
// 실제: controller/health.go:setApplicationHealth()
func AssessApplicationHealth(resources []ManagedResource) (HealthStatusCode, []ResourceHealthResult) {
	appHealth := HealthStatusHealthy
	results := make([]ResourceHealthResult, 0, len(resources))

	for _, mr := range resources {
		// Hook 리소스는 헬스 집계에서 제외
		// 실제: "if res.Live != nil && (hookutil.IsHook(res.Live) || ignore.Ignore(res.Live)) { continue }"
		if mr.IsHook {
			continue
		}

		var healthStatus *HealthStatus
		if mr.Resource == nil {
			// 리소스가 클러스터에 없음
			healthStatus = &HealthStatus{Status: HealthStatusMissing}
		} else {
			var err error
			healthStatus, err = GetResourceHealth(mr.Resource, mr.HealthOverride)
			if err != nil {
				healthStatus = &HealthStatus{Status: HealthStatusUnknown, Message: err.Error()}
			}
		}

		if healthStatus == nil {
			// 헬스 개념 없는 리소스 (ConfigMap 등)
			results = append(results, ResourceHealthResult{
				Resource: mr.Resource,
				Health:   nil,
			})
			continue
		}

		results = append(results, ResourceHealthResult{
			Resource: mr.Resource,
			Health:   healthStatus,
		})

		// Missing 리소스는 앱 헬스에 영향 안 줌
		// 실제: "if res.Live == nil && healthStatus.Status == health.HealthStatusMissing { continue }"
		if mr.Resource == nil && healthStatus.Status == HealthStatusMissing {
			continue
		}

		// 가장 나쁜 상태 추적 (IsWorse 사용)
		// 실제: controller/health.go:85-87
		if IsWorse(appHealth, healthStatus.Status) {
			appHealth = healthStatus.Status
		}
	}

	return appHealth, results
}

type ResourceHealthResult struct {
	Resource *Resource
	Health   *HealthStatus
}

// ─────────────────────────────────────────────
// 출력 헬퍼
// ─────────────────────────────────────────────

func healthIcon(status HealthStatusCode) string {
	switch status {
	case HealthStatusHealthy:
		return "[Healthy   ]"
	case HealthStatusProgressing:
		return "[Progress  ]"
	case HealthStatusDegraded:
		return "[Degraded  ]"
	case HealthStatusSuspended:
		return "[Suspended ]"
	case HealthStatusMissing:
		return "[Missing   ]"
	default:
		return "[Unknown   ]"
	}
}

func printScenario(title string) {
	fmt.Printf("\n%s\n%s\n", strings.Repeat("=", 65), title)
}

func printResourceHealth(name, kind string, health *HealthStatus, override bool) {
	overrideStr := ""
	if override {
		overrideStr = " [Lua override]"
	}
	if health == nil {
		fmt.Printf("  %-30s %s (헬스 개념 없음)%s\n",
			fmt.Sprintf("%s/%s", kind, name), "[N/A       ]", overrideStr)
		return
	}
	fmt.Printf("  %-30s %s %s%s\n",
		fmt.Sprintf("%s/%s", kind, name),
		healthIcon(health.Status),
		health.Message,
		overrideStr)
}

// ─────────────────────────────────────────────
// 테스트 리소스 생성 헬퍼
// ─────────────────────────────────────────────

func int32Ptr(i int32) *int32 { return &i }

func makeDeployment(name string, replicas int32, updatedReplicas, availableReplicas int32,
	generation, observedGen int64, paused bool, conditions []Condition) *Resource {
	return &Resource{
		GVK:       GVK{Group: "apps", Version: "v1", Kind: "Deployment"},
		Name:      name,
		Namespace: "default",
		Spec: ResourceSpec{
			Replicas: int32Ptr(replicas),
			Paused:   paused,
		},
		Status: ResourceStatus{
			UpdatedReplicas:    updatedReplicas,
			AvailableReplicas:  availableReplicas,
			Generation:         generation,
			ObservedGeneration: observedGen,
			Conditions:         conditions,
		},
	}
}

func makePod(name, phase, restartPolicy string, containerStatuses []ContainerStatus) *Resource {
	return &Resource{
		GVK:       GVK{Group: "", Version: "v1", Kind: "Pod"},
		Name:      name,
		Namespace: "default",
		Status: ResourceStatus{
			PodPhase:          phase,
			RestartPolicy:     restartPolicy,
			ContainerStatuses: containerStatuses,
		},
	}
}

func makePVC(name, phase string) *Resource {
	return &Resource{
		GVK:       GVK{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
		Name:      name,
		Namespace: "default",
		Spec:      ResourceSpec{PVCPhase: phase},
	}
}

func makeJob(name string, conditions []Condition) *Resource {
	return &Resource{
		GVK:       GVK{Group: "batch", Version: "v1", Kind: "Job"},
		Name:      name,
		Namespace: "default",
		Status:    ResourceStatus{Conditions: conditions},
	}
}

func makeStatefulSet(name string, replicas, readyReplicas int32, generation, observedGen int64,
	currentRev, updateRev string) *Resource {
	return &Resource{
		GVK:       GVK{Group: "apps", Version: "v1", Kind: "StatefulSet"},
		Name:      name,
		Namespace: "default",
		Spec:      ResourceSpec{Replicas: int32Ptr(replicas)},
		Status: ResourceStatus{
			ReadyReplicas:      readyReplicas,
			Generation:         generation,
			ObservedGeneration: observedGen,
			CurrentRevision:    currentRev,
			UpdateRevision:     updateRev,
		},
	}
}

// ─────────────────────────────────────────────
// main — 시나리오 실행
// ─────────────────────────────────────────────

func main() {
	fmt.Println("=== Argo CD 헬스 평가 엔진 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: gitops-engine/pkg/health/health.go")
	fmt.Println()
	fmt.Println("GetResourceHealth() 우선순위:")
	fmt.Println("  1. DeletionTimestamp != nil → Progressing")
	fmt.Println("  2. healthOverride (Lua 스크립트) → 결과 반환")
	fmt.Println("  3. 내장 GVK별 체크")
	fmt.Println("  4. nil → 헬스 개념 없음 (ConfigMap 등)")

	// ─────────────────────────────────────────────
	// 시나리오 1: DeletionTimestamp
	// ─────────────────────────────────────────────
	printScenario("시나리오 1: DeletionTimestamp → Progressing")
	fmt.Println("  삭제 중인 리소스는 무조건 Progressing")

	now := time.Now()
	deletingDeploy := makeDeployment("deleting-app", 3, 3, 3, 1, 1, false, nil)
	deletingDeploy.DeletionTimestamp = &now

	health, _ := GetResourceHealth(deletingDeploy, nil)
	printResourceHealth("deleting-app", "Deployment", health, false)

	// ─────────────────────────────────────────────
	// 시나리오 2: Deployment 헬스 체크
	// ─────────────────────────────────────────────
	printScenario("시나리오 2: Deployment 헬스 체크 (health_deployment.go)")

	cases := []struct {
		desc string
		r    *Resource
	}{
		{
			"Healthy — replicas=3, updated=3, available=3",
			makeDeployment("healthy-app", 3, 3, 3, 1, 1, false, nil),
		},
		{
			"Paused — spec.paused=true",
			makeDeployment("paused-app", 3, 3, 3, 1, 1, true, nil),
		},
		{
			"Progressing — updated=1/3 (롤아웃 중)",
			makeDeployment("rolling-app", 3, 1, 1, 1, 1, false, nil),
		},
		{
			"Progressing — available=2 < updated=3",
			makeDeployment("scaling-app", 3, 3, 2, 1, 1, false, nil),
		},
		{
			"Progressing — generation(2) > observedGeneration(1)",
			makeDeployment("new-spec-app", 3, 3, 3, 2, 1, false, nil),
		},
		{
			"Degraded — ProgressDeadlineExceeded",
			makeDeployment("stuck-app", 3, 1, 1, 1, 1, false, []Condition{
				{Type: "Progressing", Reason: "ProgressDeadlineExceeded", Status: "False"},
			}),
		},
	}

	for _, c := range cases {
		health, _ := GetResourceHealth(c.r, nil)
		fmt.Printf("  %-50s → %s\n", c.desc, health)
	}

	// ─────────────────────────────────────────────
	// 시나리오 3: Pod 헬스 체크
	// ─────────────────────────────────────────────
	printScenario("시나리오 3: Pod 헬스 체크 (health_pod.go)")
	fmt.Println("  RestartPolicy=Always: Running+Ready → Healthy")
	fmt.Println("  RestartPolicy=OnFailure/Never: Running → Progressing (hook pod)")

	podCases := []struct {
		desc string
		r    *Resource
	}{
		{
			"Pending — 스케줄링 대기",
			makePod("pending-pod", "Pending", "Always", nil),
		},
		{
			"Running+Ready — 정상",
			makePod("ready-pod", "Running", "Always", []ContainerStatus{
				{Name: "app", WaitingReason: ""},
			}),
		},
		{
			"Running+CrashLoopBackOff — Degraded",
			makePod("crash-pod", "Running", "Always", []ContainerStatus{
				{Name: "app", WaitingReason: "CrashLoopBackOff", WaitingMessage: "back-off restarting failed container"},
			}),
		},
		{
			"Running+ImagePullBackOff — Degraded",
			makePod("imagepull-pod", "Running", "Always", []ContainerStatus{
				{Name: "app", WaitingReason: "ImagePullBackOff", WaitingMessage: "Back-off pulling image"},
			}),
		},
		{
			"Running+OnFailure — Progressing (hook pod)",
			makePod("hook-pod", "Running", "OnFailure", nil),
		},
		{
			"Failed — Degraded",
			makePod("failed-pod", "Failed", "Always", nil),
		},
		{
			"Succeeded — Healthy",
			makePod("completed-pod", "Succeeded", "Never", nil),
		},
	}

	for _, c := range podCases {
		health, _ := GetResourceHealth(c.r, nil)
		fmt.Printf("  %-45s → %s\n", c.desc, health)
	}

	// ─────────────────────────────────────────────
	// 시나리오 4: PVC, Job, StatefulSet
	// ─────────────────────────────────────────────
	printScenario("시나리오 4: PVC, Job, StatefulSet 헬스")

	// PVC
	fmt.Println("\n  PVC (health_pvc.go):")
	pvcCases := []struct {
		phase string
		want  HealthStatusCode
	}{
		{"Bound", HealthStatusHealthy},
		{"Pending", HealthStatusProgressing},
		{"Lost", HealthStatusDegraded},
	}
	for _, c := range pvcCases {
		r := makePVC("my-pvc-"+strings.ToLower(c.phase), c.phase)
		health, _ := GetResourceHealth(r, nil)
		fmt.Printf("    PVC phase=%-10s → %s\n", c.phase, health)
	}

	// Job
	fmt.Println("\n  Job (health_job.go):")
	jobCases := []struct {
		desc string
		conds []Condition
	}{
		{"실행 중 (Conditions 없음)", nil},
		{"완료", []Condition{{Type: "Complete", Status: "True"}}},
		{"실패", []Condition{{Type: "Failed", Status: "True", Message: "BackoffLimitExceeded"}}},
		{"일시 중단", []Condition{{Type: "Suspended", Status: "True"}}},
	}
	for _, c := range jobCases {
		r := makeJob("my-job", c.conds)
		health, _ := GetResourceHealth(r, nil)
		fmt.Printf("    %-25s → %s\n", c.desc, health)
	}

	// StatefulSet
	fmt.Println("\n  StatefulSet (health_statefulset.go):")
	stsCases := []struct {
		desc          string
		replicas      int32
		ready         int32
		gen           int64
		obsGen        int64
		currRev, upRev string
	}{
		{"Healthy (ready=3/3, rev=curr)", 3, 3, 1, 1, "v1", "v1"},
		{"Progressing (ready=1/3)", 3, 1, 1, 1, "v1", "v1"},
		{"Progressing (update 진행 중)", 3, 3, 1, 1, "v1", "v2"},
		{"Progressing (spec 미관찰)", 3, 3, 2, 1, "v1", "v1"},
	}
	for _, c := range stsCases {
		r := makeStatefulSet("my-sts", c.replicas, c.ready, c.gen, c.obsGen, c.currRev, c.upRev)
		health, _ := GetResourceHealth(r, nil)
		fmt.Printf("    %-40s → %s\n", c.desc, health)
	}

	// ─────────────────────────────────────────────
	// 시나리오 5: 헬스 없는 리소스 (ConfigMap)
	// ─────────────────────────────────────────────
	printScenario("시나리오 5: 헬스 개념 없는 리소스")
	fmt.Println("  ConfigMap, Secret, Role 등은 헬스 체크 없음 → nil 반환")

	configMap := &Resource{
		GVK:  GVK{Group: "", Version: "v1", Kind: "ConfigMap"},
		Name: "app-config",
	}
	health5, _ := GetResourceHealth(configMap, nil)
	printResourceHealth("app-config", "ConfigMap", health5, false)

	// ─────────────────────────────────────────────
	// 시나리오 6: Lua healthOverride 시뮬레이션
	// ─────────────────────────────────────────────
	printScenario("시나리오 6: Lua healthOverride (CRD 커스텀 헬스)")
	fmt.Println("  Argo CD는 resourceOverrides로 CRD에 Lua 스크립트 헬스 체크 지원")
	fmt.Println("  실제: controller/health.go → util/lua.ResourceHealthOverrides()")

	// ArgoCD Application CRD 커스텀 헬스 체크 시뮬레이션
	argoAppOverride := &LuaHealthOverride{
		ScriptName: "argoproj.io/Application",
		OverrideFunc: func(r *Resource) (*HealthStatus, error) {
			// 실제 ArgoCD의 Application 헬스 체크 Lua 로직을 Go로 표현
			// operationState.phase 확인
			phase, ok := r.Annotations["health.phase"]
			if !ok {
				return &HealthStatus{Status: HealthStatusHealthy}, nil
			}
			switch phase {
			case "Running":
				return &HealthStatus{Status: HealthStatusProgressing, Message: "Operation in progress"}, nil
			case "Failed", "Error":
				return &HealthStatus{Status: HealthStatusDegraded, Message: "Operation failed"}, nil
			default:
				return &HealthStatus{Status: HealthStatusHealthy}, nil
			}
		},
	}

	argoApps := []*Resource{
		{
			GVK: GVK{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"},
			Name: "my-app-synced",
			Annotations: map[string]string{"health.phase": "Succeeded"},
		},
		{
			GVK: GVK{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"},
			Name: "my-app-syncing",
			Annotations: map[string]string{"health.phase": "Running"},
		},
		{
			GVK: GVK{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"},
			Name: "my-app-failed",
			Annotations: map[string]string{"health.phase": "Failed"},
		},
	}

	for _, app := range argoApps {
		health, _ := GetResourceHealth(app, argoAppOverride)
		printResourceHealth(app.Name, "Application", health, true)
	}

	// ─────────────────────────────────────────────
	// 시나리오 7: 앱 레벨 헬스 집계 (IsWorse)
	// ─────────────────────────────────────────────
	printScenario("시나리오 7: 앱 레벨 헬스 집계 (setApplicationHealth)")
	fmt.Println("  모든 리소스 중 가장 나쁜 상태 = 앱 헬스")
	fmt.Println("  healthOrder: Healthy > Suspended > Progressing > Missing > Degraded > Unknown")
	fmt.Println()

	appResources := []ManagedResource{
		{
			Resource: makeDeployment("frontend", 3, 3, 3, 1, 1, false, nil),
		},
		{
			Resource: makeDeployment("backend", 3, 3, 3, 1, 1, false, nil),
		},
		{
			Resource: makeStatefulSet("postgres", 3, 3, 1, 1, "v1", "v1"),
		},
		{
			Resource: makePVC("data-pvc", "Bound"),
		},
		{
			Resource: makeDeployment("worker", 3, 1, 1, 1, 1, false, nil), // Progressing
		},
		{
			// Hook은 헬스 집계 제외
			Resource: makeJob("presync-hook", nil),
			IsHook:   true,
		},
	}

	fmt.Println("  리소스 목록:")
	for _, mr := range appResources {
		if mr.Resource == nil {
			continue
		}
		health, _ := GetResourceHealth(mr.Resource, mr.HealthOverride)
		hookStr := ""
		if mr.IsHook {
			hookStr = " [Hook: 집계 제외]"
		}
		printResourceHealth(mr.Resource.Name, mr.Resource.Kind, health, false)
		if hookStr != "" {
			fmt.Printf("    ↑ %s\n", hookStr)
		}
	}

	appHealth, _ := AssessApplicationHealth(appResources)
	fmt.Printf("\n  앱 전체 헬스: %s %s\n", healthIcon(appHealth), appHealth)
	fmt.Println("  (worker Deployment가 Progressing이므로 앱 전체도 Progressing)")

	// IsWorse 확인
	fmt.Println()
	printScenario("IsWorse 함수 예시")
	fmt.Println("  현재 앱 헬스 순서:")
	for i, code := range healthOrder {
		fmt.Printf("  %d. %s\n", i, code)
	}
	fmt.Println()
	pairs := [][2]HealthStatusCode{
		{HealthStatusHealthy, HealthStatusProgressing},
		{HealthStatusProgressing, HealthStatusDegraded},
		{HealthStatusDegraded, HealthStatusHealthy},
		{HealthStatusSuspended, HealthStatusMissing},
	}
	for _, p := range pairs {
		worse := IsWorse(p[0], p[1])
		fmt.Printf("  IsWorse(current=%s, new=%s) = %v\n", p[0], p[1], worse)
	}

	fmt.Println()
	fmt.Println(strings.Repeat("=", 65))
	fmt.Println("헬스 평가 핵심 포인트:")
	fmt.Println("  1. GetResourceHealth() 우선순위: DeletionTimestamp > Override > BuiltIn > nil")
	fmt.Println("  2. healthOrder: Healthy > Suspended > Progressing > Missing > Degraded > Unknown")
	fmt.Println("  3. IsWorse(): 앱 헬스는 모든 리소스 중 최악의 상태")
	fmt.Println("  4. Hook 리소스는 앱 헬스 집계에서 제외")
	fmt.Println("  5. 헬스 없는 리소스(ConfigMap 등)는 nil 반환, 집계 무시")
	fmt.Println("  6. Lua override: CRD에 커스텀 헬스 체크 정의 가능")
}
